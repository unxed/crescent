package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/unxed/crescent/internal/appserver"
)

// A Journal records what each goal's model is doing, so that a run left
// unattended can be read back later. Editing is out of scope by design: to
// change a goal you stop crescent and use Codex, or you change the instruction
// on GitHub. This is a window, not a workbench.
//
// Each thread gets its own file, named by a human-chosen label where one is
// known, so the directory is browsable. A combined feed interleaves everything
// for a single-file overview.
type Journal struct {
	dir string

	mu       sync.Mutex
	perGoal  map[string]*os.File
	labels   map[string]string
	combined *os.File
	// server is app-server's own diagnostics, kept apart from the human log.
	// On a live run it produced 15768 lines against 96 of ours: mixed together,
	// the journal a person reads was 99.4% machine noise.
	server *os.File
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
		dir:     dir,
		perGoal: map[string]*os.File{},
		labels:  map[string]string{},
	}
	j.combined, _ = openRotating(filepath.Join(dir, "all.log"))
	j.server, _ = openRotating(filepath.Join(dir, "app-server.log"))
	return j, nil
}

// Label gives a thread a readable name for its file. Called when the goal is
// chosen, before any activity arrives.
func (j *Journal) Label(threadID, label string) {
	j.mu.Lock()
	j.labels[threadID] = label
	j.mu.Unlock()
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
			_, _ = f.WriteString(line)
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
func (j *Journal) fileFor(threadID string) *os.File {
	if f, ok := j.perGoal[threadID]; ok {
		return f
	}
	name := j.labels[threadID]
	if name == "" {
		name = threadID
	}
	f, err := openRotating(filepath.Join(j.dir, safeName(name)+".log"))
	if err != nil {
		return nil
	}
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
	labels := make([]string, 0, len(j.labels))
	for _, l := range j.labels {
		if l != "" {
			labels = append(labels, l)
		}
	}
	j.mu.Unlock()

	sort.Strings(labels)
	return append([]string{""}, labels...) // "" is the combined feed
}

// TailOf returns the last n lines of one goal's log, or of the combined feed
// when label is empty. This is what lets the window show a single goal's work
// in full instead of the interleaved summary.
func (j *Journal) TailOf(label string, n int) string {
	name := "all.log"
	if label != "" {
		name = safeName(label) + ".log"
	}
	return tailFile(filepath.Join(j.dir, name), n)
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
}

const maxLog = 8 << 20

// openRotating opens a log for appending, rolling it aside once it passes the
// size cap so an unattended run of days cannot fill the disk. One generation is
// kept.
func openRotating(path string) (*os.File, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLog {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

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
