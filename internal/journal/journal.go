package journal

import (
	"fmt"
	"os"
	"path/filepath"
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
	return j, nil
}

// Label gives a thread a readable name for its file. Called when the goal is
// chosen, before any activity arrives.
func (j *Journal) Label(threadID, label string) {
	j.mu.Lock()
	j.labels[threadID] = label
	j.mu.Unlock()
}

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
	line := fmt.Sprintf("%s  [%s] %s\n", time.Now().Format("15:04:05"), kind, text)

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
			time.Now().Format("15:04:05"), short(label, 24), kind, text)
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
