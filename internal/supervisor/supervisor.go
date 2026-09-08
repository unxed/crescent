package supervisor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/unxed/crescent/internal/appserver"
	"github.com/unxed/crescent/internal/codexcli"
	"github.com/unxed/crescent/internal/journal"
	"github.com/unxed/crescent/internal/pins"
)

// State is what the supervisor is doing, in one word for a tray tooltip.
type State string

// Supervisor states.
const (
	StateIdle       State = "простаиваю"
	StateWaitingApp State = "жду закрытия приложения"
	StateLimited    State = "жду сброса лимита"
	StateWorking    State = "веду цели"
	StateNoGoals    State = "нет закреплённых целей"
)

// Status is a snapshot for whoever is watching — a window, a tray, a terminal.
type Status struct {
	State    State
	Message  string
	Until    time.Time // when a wait ends, if known
	Pinned   int
	Restarts int
	Since    time.Time
}

// Line is a one-line summary.
func (s Status) Line() string {
	switch s.State {
	case StateLimited:
		if !s.Until.IsZero() {
			return fmt.Sprintf("жду сброса лимита до %s", s.Until.Local().Format("15:04"))
		}
	case StateWorking:
		return fmt.Sprintf("веду %d целей", s.Pinned)
	}
	if s.Message != "" {
		return string(s.State) + ": " + s.Message
	}
	return string(s.State)
}

// Supervisor keeps the pinned goals moving. It owns no UI: a window drives it
// through Pins and reads it through Status, a terminal does the same.
type Supervisor struct {
	client *appserver.Client
	pins   *pins.Pins
	jour   *journal.Journal
	prompt string
	poll   time.Duration

	mu       sync.Mutex
	status   Status
	started  map[string]time.Time // last time we pushed each goal
	onChange func(Status)
}

// Options configure a supervisor.
type Options struct {
	Client  *appserver.Client
	Pins    *pins.Pins
	Journal *journal.Journal
	Prompt  string
	Poll    time.Duration
	// OnChange is called whenever the status changes, on the supervisor's
	// goroutine. A UI uses it to refresh without polling.
	OnChange func(Status)
}

// New builds a supervisor.
func New(o Options) *Supervisor {
	poll := o.Poll
	if poll <= 0 {
		poll = 30 * time.Second
	}
	prompt := o.Prompt
	if prompt == "" {
		prompt = "Продолжай работу над текущей целью."
	}
	return &Supervisor{
		client:   o.Client,
		pins:     o.Pins,
		jour:     o.Journal,
		prompt:   prompt,
		poll:     poll,
		started:  map[string]time.Time{},
		onChange: o.OnChange,
		status:   Status{State: StateIdle, Since: time.Now()},
	}
}

// Status returns the current snapshot. Safe from any goroutine.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *Supervisor) set(state State, msg string, until time.Time) {
	s.mu.Lock()
	if s.status.State != state || s.status.Message != msg {
		s.status.Since = time.Now()
	}
	s.status.State, s.status.Message, s.status.Until = state, msg, until
	s.status.Pinned = s.pins.Count()
	snap := s.status
	cb := s.onChange
	s.mu.Unlock()
	if cb != nil {
		cb(snap)
	}
}

func (s *Supervisor) bumpRestart() {
	s.mu.Lock()
	s.status.Restarts++
	s.mu.Unlock()
}

// Run drives the pinned goals until the context is cancelled. Each pass:
//  1. wait for the ChatGPT application to be closed — only one owner per thread;
//  2. read the account limit; an unknown limit is treated as closed, never open;
//  3. for every pinned, restartable goal that is not already running, push it.
func (s *Supervisor) Run(ctx context.Context) error {
	s.note("наблюдение запущено")
	for {
		if ctx.Err() != nil {
			s.set(StateIdle, "остановлено", time.Time{})
			return nil
		}

		if s.pins.Count() == 0 {
			s.set(StateNoGoals, "", time.Time{})
			if !s.sleep(ctx) {
				return nil
			}
			continue
		}

		if !s.awaitClosedApp(ctx) {
			return nil
		}

		limits, err := s.client.RateLimitsWithRetry(ctx, 3)
		if err != nil {
			// Unknown is not permission.
			s.set(StateLimited, "состояние лимита не выяснено", time.Time{})
			if !s.sleep(ctx) {
				return nil
			}
			continue
		}
		if limited, at := limits.Exhausted(); limited {
			s.set(StateLimited, "", at)
			if !s.sleep(ctx) {
				return nil
			}
			continue
		}

		s.set(StateWorking, "", time.Time{})
		s.restartPinned(ctx)

		if !s.sleep(ctx) {
			return nil
		}
	}
}

// restartPinned pushes each pinned goal that has stopped. Status is re-read
// every pass, never remembered: a goal may have finished, been paused by hand,
// or been taken over by the application between passes.
func (s *Supervisor) restartPinned(ctx context.Context) {
	for _, id := range s.pins.IDs() {
		if ctx.Err() != nil {
			return
		}
		label := s.pins.Label(id)
		s.jour.Label(id, label)

		goal, err := s.client.Goal(ctx, id)
		if err != nil {
			s.note2(id, "состояние не прочитано: "+err.Error())
			continue
		}
		switch {
		case !goal.Set():
			continue
		case !appserver.Restartable(goal.Status):
			continue
		case strings.EqualFold(goal.Status, appserver.StatusActive):
			continue // already moving
		case time.Since(s.started[id]) < 2*time.Minute:
			continue // just pushed; let the turn appear
		}
		if err := s.client.Restart(ctx, id, s.prompt); err != nil {
			s.note2(id, "запустить не удалось: "+err.Error())
			continue
		}
		s.started[id] = time.Now()
		s.bumpRestart()
		s.note2(id, "перезапущено (было "+goal.Status+")")
	}
}

// awaitClosedApp blocks until the ChatGPT application is gone, reporting the
// wait so a UI can show it. Returns false if the context is cancelled.
func (s *Supervisor) awaitClosedApp(ctx context.Context) bool {
	running, name := codexcli.AppRunning()
	if !running {
		return true
	}
	s.set(StateWaitingApp, "закройте "+name+" целиком", time.Time{})
	s.note("жду закрытия приложения " + name)
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(3 * time.Second):
			if r, _ := codexcli.AppRunning(); !r {
				s.note("приложение закрыто — продолжаю")
				return true
			}
		}
	}
}

// sleep waits one poll interval in short hops. Short hops matter: a laptop that
// suspends for two hours must wake up and re-check the wall clock, not keep
// counting down a single long timer.
func (s *Supervisor) sleep(ctx context.Context) bool {
	deadline := time.Now().Add(s.poll)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
	return true
}

func (s *Supervisor) note(text string)      { s.jour.Note("", text) }
func (s *Supervisor) note2(id, text string) { s.jour.Note(id, text) }

// Snapshot lists the pinned goals with their current state, for a window to
// render. It is a read: safe while Run is going.
type GoalView struct {
	ThreadID string
	Label    string
	Status   string
	Cwd      string
}

// Goals returns everything the server offers, marked with whether it is pinned,
// so a window can show the full list with checkboxes.
func (s *Supervisor) Goals(ctx context.Context) ([]GoalView, error) {
	cands, err := s.client.Candidates(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]GoalView, 0, len(cands))
	for _, c := range cands {
		out = append(out, GoalView{
			ThreadID: c.Thread.ID,
			Label:    c.Thread.Label(),
			Status:   c.Goal.Status,
			Cwd:      c.Thread.Cwd,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}
