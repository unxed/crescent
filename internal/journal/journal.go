package journal

import (
	"fmt"
	"github.com/unxed/crescent/internal/appserver"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// A Journal records what each goal's model is doing, so that a run left
// unattended can be read back later. Editing is out of scope by design: to
// change a goal you stop crescent and use Codex, or you change the instruction
// on GitHub. This is a window, not a workbench.
//
// Each thread gets its own file, named by a human-chosen label where one is
// known, so the directory is browsable. A combined feed interleaves everything
// for a single-file overview.
// rotatingFile is an append-only log that rolls itself aside once it passes the
// size cap.
//
// Rotation used to be checked only when the file was opened, so a process that
// stays up for a day wrote into one file forever: a live run produced a 2 GB
// app-server.log. The size has to be watched as it is written, not once at the
// start.
type rotatingFile struct {
	path string
	f    *os.File
	size int64
}

func newRotatingFile(path string) (*rotatingFile, error) {
	r := &rotatingFile{path: path}
	return r, r.open()
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	r.f = f
	if fi, err := f.Stat(); err == nil {
		r.size = fi.Size()
	}
	return nil
}

// Write appends, rolling over when the cap is passed. One generation is kept,
// so a log can never occupy more than twice the cap.
func (r *rotatingFile) Write(p []byte) (int, error) {
	if r == nil || r.f == nil {
		return len(p), nil
	}
	if r.size+int64(len(p)) > maxLog {
		_ = r.f.Close()
		_ = os.Rename(r.path, r.path+".1")
		r.size = 0
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotatingFile) Close() error {
	if r == nil || r.f == nil {
		return nil
	}
	return r.f.Close()
}

type Journal struct {
	dir     string
	started time.Time

	mu      sync.Mutex
	perGoal map[string]*rotatingFile
	labels  map[string]string
	// touched records which goals produced anything during this run, so a
	// quiet goal can be told from one that is simply showing history. Matching
	// on a timestamp instead was fragile: two runs in the same second collided.
	touched  map[string]bool
	combined *rotatingFile
	// server is app-server's own diagnostics, kept apart from the human log.
	// On a live run it produced 15768 lines against 96 of ours: mixed together,
	// the journal a person reads was 99.4% machine noise.
	server *rotatingFile
	// wire is the verbatim protocol, opened only when asked for: it is large
	// and it is for investigation, not for reading day to day.
	wire *rotatingFile
}

// Dir is where journals live: a subdirectory of the user cache.
func Dir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "crescent", "journal"), nil
}

// Open prepares the journal directory and the combined feed.
func Open() (*Journal, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	j := &Journal{
		started: time.Now(),
		dir:     dir,
		perGoal: map[string]*rotatingFile{},
		labels:  map[string]string{},
		touched: map[string]bool{},
	}
	j.combined, _ = newRotatingFile(filepath.Join(dir, "all.log"))
	j.server, _ = newRotatingFile(filepath.Join(dir, "app-server.log"))
	return j, nil
}

// Label gives a thread a readable name for its file. Called when the goal is
// chosen, before any activity arrives.
func (j *Journal) Label(threadID, label string) {
	j.mu.Lock()
	j.labels[threadID] = label
	j.mu.Unlock()
}

// OpenWire starts recording the raw protocol into protocol.log.
func (j *Journal) OpenWire() (string, error) {
	path := filepath.Join(j.dir, "protocol.log")
	f, err := newRotatingFile(path)
	if err != nil {
		return "", err
	}
	j.mu.Lock()
	j.wire = f
	j.mu.Unlock()
	return path, nil
}

// Wire records one line of the protocol, in the direction given.
func (j *Journal) Wire(dir, line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.wire == nil {
		return
	}
	fmt.Fprintf(j.wire, "%s %s %s\n", time.Now().Format("15:04:05.000"), dir, line)
}

// Server records one line of app-server's own logging. It goes to its own file,
// and only warnings and errors reach the human journal: everything else is
// INFO-level tracing that no person is reading.
func (j *Journal) Server(line string) {
	line = stripANSI(line)
	if line == "" {
		return
	}
	j.mu.Lock()
	if j.server != nil {
		fmt.Fprintf(j.server, "%s  %s\n", time.Now().Format("01-02 15:04:05"), line)
	}
	j.mu.Unlock()

	// A line that names a thread belongs in that goal's own log. Without this
	// the diagnosis sat in the combined feed while the goal's log said nothing
	// was happening — the answer was on disk and the screen showed silence.
	if id := appserver.ThreadIDIn(line); id != "" {
		j.write(id, "app-server", line)
		return
	}
	if strings.Contains(line, " ERROR ") || strings.Contains(line, " WARN ") {
		j.write("", "app-server", line)
	}
}

// stripANSI removes the colour escapes tracing writes when it thinks it is
// talking to a terminal. Rendered in a text view they came out as a wall of
// unreadable boxes.
func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// Note records a line crescent itself wants in the journal — a restart, a wait
// — attributed to a goal.
func (j *Journal) Note(threadID, text string) {
	j.write(threadID, "crescent", text)
}

// Record files one Activity into that goal's log and the combined feed.
func (j *Journal) Record(a appserver.Activity) {
	j.write(a.ThreadID, a.Kind, a.Text)
}

func (j *Journal) write(threadID, kind, text string) {
	if text == "" {
		return
	}
	text = collapse(text)
	now := time.Now()
	line := fmt.Sprintf("%s  [%s] %s\n", now.Format("01-02 15:04:05"), kind, text)

	j.mu.Lock()
	defer j.mu.Unlock()

	// A per-goal file is opened only for a real thread. Notifications without a
	// thread id — and crescent's own top-level notes — go to the combined feed
	// only, so a stray "goal.log" never appears.
	if threadID != "" {
		if f := j.fileFor(threadID); f != nil {
			_, _ = f.Write([]byte(line))
			if l := j.labels[threadID]; l != "" {
				j.touched[l] = true
			}
		}
	}
	if j.combined != nil {
		label := j.labels[threadID]
		if label == "" {
			label = short(threadID)
		}
		fmt.Fprintf(j.combined, "%s  %-24s [%s] %s\n",
			now.Format("01-02 15:04:05"), middle(label, 24), kind, text)
	}
}

// fileFor returns the log for a thread, opening it on first use. Called with
// the lock held.
func (j *Journal) fileFor(threadID string) *rotatingFile {
	if f, ok := j.perGoal[threadID]; ok {
		return f
	}
	name := j.labels[threadID]
	if name == "" {
		name = threadID
	}
	f, err := newRotatingFile(filepath.Join(j.dir, safeName(name)+".log"))
	if err != nil {
		return nil
	}
	// A session marker, written once per goal per run. Without it the newest
	// line in a quiet goal's log is whatever happened days ago, and with
	// newest-first order it sits at the top looking like current news — which
	// is exactly how a day-old "app-server закрыл поток" was read as a live
	// failure.
	fmt.Fprintf(f, "%s  [сеанс] --- запуск crescent ---\n", j.started.Format("01-02 15:04:05"))
	j.perGoal[threadID] = f
	return f
}

// Path returns the directory, for pointing a user at it.
func (j *Journal) Path() string { return j.dir }

// Views lists what can be shown in the window: the combined feed first, then
// one entry per goal that has a log. Labels come from the pins, so the list
// reads the way the checkboxes do.
func (j *Journal) Views() []string {
	j.mu.Lock()
	// Distinct labels. Two goals can carry the same name — a chat recreated
	// under its old name leaves the dead thread pinned alongside the new one —
	// and a repeated entry made the switcher stick: it finds the first match,
	// steps to the next index, and lands on the same name for ever.
	seen := map[string]bool{}
	labels := make([]string, 0, len(j.labels))
	for _, l := range j.labels {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		labels = append(labels, l)
	}
	j.mu.Unlock()

	sort.Strings(labels)
	return append([]string{""}, labels...) // "" is the combined feed
}

// TailOf returns the last n lines of one goal's log, or of the combined feed
// when label is empty. This is what lets the window show a single goal's work
// in full instead of the interleaved summary.
//
// When nothing happened for this goal during the current run, that is said in
// so many words. Otherwise the newest line is whatever happened days ago and,
// shown newest-first, reads as current news.
func (j *Journal) TailOf(label string, n int) string {
	name := "all.log"
	if label != "" {
		name = safeName(label) + ".log"
	}
	text := tailFile(filepath.Join(j.dir, name), n)
	if label == "" || text == "" {
		return text
	}
	j.mu.Lock()
	touched := j.touched[label]
	j.mu.Unlock()
	if !touched {
		return "(за этот запуск для цели ничего не происходило — ниже прошлые записи)\n\n" + text
	}
	return text
}

// Tail returns roughly the last n lines of the combined feed, so the window can
// show recent activity without the user leaving the app.
func (j *Journal) Tail(n int) string { return j.TailOf("", n) }

// tailFile reads the last n lines of a file. Reading it whole is fine: the log
// is size-capped at a few megabytes and rolled over past that.
func tailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	// Newest first: "what is happening now" is the question the pane is opened
	// to answer, and it should not require scrolling to the bottom.
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return strings.Join(lines, "\n")
}

// Close flushes and closes every file.
func (j *Journal) Close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, f := range j.perGoal {
		_ = f.Close()
	}
	if j.combined != nil {
		_ = j.combined.Close()
	}
	if j.wire != nil {
		_ = j.wire.Close()
	}
}

const maxLog = 8 << 20

// safeName turns a chat label into a filename that will not surprise anyone.
func safeName(s string) string {
	const bad = `/\:*?"<>|` + "\x00"
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 32 {
			continue
		}
		replaced := false
		for _, b := range bad {
			if r == b {
				out = append(out, '_')
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, r)
		}
	}
	name := string(out)
	if len(name) > 80 {
		name = name[:80]
	}
	if name == "" {
		name = "goal"
	}
	return name
}

// middle shortens a label from the middle, keeping both ends.
//
// Cutting the tail threw away the only thing that distinguished one goal from
// another: "Следовать инструкции Лунобот-1" and "…-2" differ in the last
// character, and both came out as "Следовать инструкции Лу…".
func middle(s string, n int) string {
	r := []rune(s)
	if len(r) <= n || n < 5 {
		return s
	}
	head := (n - 1) / 2
	tail := n - 1 - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

// collapse squeezes runs of whitespace into single spaces: a command line
// pasted into the log carried its own indentation and read as a mess.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func short(s string, n ...int) string {
	limit := 12
	if len(n) > 0 {
		limit = n[0]
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}
