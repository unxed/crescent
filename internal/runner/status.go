package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/unxed/crescent/internal/codex"
)

// State is what the daemon is doing right now.
type State string

// The states a daemon can be in. They are deliberately few: anything watching
// this — a terminal, a tray icon — should be able to say what is happening in
// one short line.
const (
	StateStarting State = "запускается"
	StateProbing  State = "самопроверка"
	StateRunning  State = "работает"
	StateLimit    State = "ждёт сброса лимита"
	StateYielding State = "уступает, вы работаете в Codex"
	StateIdle     State = "нечего делать"
	StateStopped  State = "остановлен"
	StateFailed   State = "остановлен из-за ошибки"
)

// Status is the daemon's public state, written to disk so that another process
// can show it without talking to the daemon.
type Status struct {
	State     State     `json:"state"`
	Goal      string    `json:"goal,omitempty"`
	GoalID    string    `json:"goal_id,omitempty"`
	Since     time.Time `json:"since"`
	Until     time.Time `json:"until,omitempty"` // when the wait ends
	Message   string    `json:"message,omitempty"`
	Turns     int       `json:"turns"`
	Limits    int       `json:"limits_hit"`
	Errors    int       `json:"errors"`
	StartedAt time.Time `json:"started_at"`
	PID       int       `json:"pid"`
}

// Line is a one-line summary, the form a tray tooltip or a terminal wants.
func (s Status) Line() string {
	switch s.State {
	case StateLimit:
		if !s.Until.IsZero() {
			return "ждёт сброса лимита до " + s.Until.Local().Format("15:04") +
				" (" + time.Until(s.Until).Round(time.Minute).String() + ")"
		}
	case StateRunning:
		if s.Goal != "" {
			return "работает: " + s.Goal
		}
	}
	if s.Message != "" {
		return string(s.State) + ": " + s.Message
	}
	return string(s.State)
}

// StatusPath is where the daemon publishes its state.
func StatusPath() (string, error) {
	dir, err := codex.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "status.json"), nil
}

// StatusWriter publishes status updates. Writes are atomic, because a reader
// polling the file must never catch it half-written.
type StatusWriter struct {
	mu   sync.Mutex
	path string
}

// NewStatusWriter prepares the status file.
func NewStatusWriter() (*StatusWriter, error) {
	p, err := StatusPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	return &StatusWriter{path: p}, nil
}

// Put writes the status.
func (w *StatusWriter) Put(s Status) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	tmp := w.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		os.Rename(tmp, w.path)
	}
}

// Remove deletes the status file, so a stopped daemon does not leave a stale
// "running" behind.
func (w *StatusWriter) Remove() {
	if w != nil {
		os.Remove(w.path)
	}
}

// ReadStatus returns what the daemon last published.
func ReadStatus() (Status, bool) {
	p, err := StatusPath()
	if err != nil {
		return Status{}, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return Status{}, false
	}
	var s Status
	if json.Unmarshal(data, &s) != nil {
		return Status{}, false
	}
	// A status file whose process is gone is a leftover, not a state. A
	// recorded failure is kept as it is: why the daemon gave up outlives the
	// daemon, and is exactly what someone comes to this file for.
	if s.PID > 0 && !processAlive(s.PID) && s.State != StateFailed {
		s.State, s.Message = StateStopped, "процесс не найден"
	}
	return s, true
}
