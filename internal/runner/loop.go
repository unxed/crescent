package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/unxed/crescent/internal/codex"
)

// Loop is the daemon: it watches the sessions directory, waits out usage
// limits, and pushes goals forward one turn at a time.
type Loop struct {
	Dir    string // sessions directory
	Policy Policy
	Log    io.Writer

	// Tick is how often the loop re-examines the world while waiting. It is a
	// field so tests do not have to wait in real time.
	Tick time.Duration

	// ReadOnlyProbe runs one throwaway turn in a read-only sandbox before any
	// real work. See Run.
	ReadOnlyProbe bool

	status *StatusWriter

	// current is written by the loop and read by whatever is showing the state
	// — a tray icon polling from another goroutine, for instance.
	mu      sync.Mutex
	current Status

	// own remembers the turns crescent itself drove, so the yield check does
	// not read them as somebody else at the keyboard.
	own OwnWrites
}

// NewLoop prepares a daemon with sensible defaults.
func NewLoop(dir string, p Policy) (*Loop, error) {
	w, err := NewStatusWriter()
	if err != nil {
		return nil, err
	}
	return &Loop{
		Dir:           dir,
		Policy:        p,
		Log:           os.Stdout,
		Tick:          30 * time.Second,
		ReadOnlyProbe: true,
		status:        w,
		own:           OwnWrites{},
		current: Status{
			State:     StateStarting,
			Since:     time.Now(),
			StartedAt: time.Now(),
			PID:       os.Getpid(),
		},
	}, nil
}

func (l *Loop) setState(s State, msg string, until time.Time, goal *codex.Session) {
	l.mu.Lock()
	if l.current.State != s || l.current.Message != msg {
		l.current.Since = time.Now()
	}
	l.current.State, l.current.Message, l.current.Until = s, msg, until
	if goal != nil {
		l.current.Goal, l.current.GoalID = goal.Label(), goal.ID
	} else {
		l.current.Goal, l.current.GoalID = "", ""
	}
	snapshot := l.current
	l.mu.Unlock()

	l.status.Put(snapshot)
	l.logf("%s", snapshot.Line())
}

// bump applies a counter change under the same lock as the state.
func (l *Loop) bump(f func(*Status)) {
	l.mu.Lock()
	f(&l.current)
	l.mu.Unlock()
}

func (l *Loop) logf(format string, a ...any) {
	if l.Log == nil {
		return
	}
	fmt.Fprintf(l.Log, "%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

// Run drives goals until the context is cancelled.
//
// The first thing it does is a self-check: one turn in a read-only sandbox. The
// question "does resume actually work against a live session on this machine?"
// has never been answered, and answering it by starting to edit repositories
// unattended is the wrong way round. A read-only turn proves the CLI, the
// authentication and the event stream, and cannot touch anything.
func (l *Loop) Run(ctx context.Context) error {
	if l.ReadOnlyProbe {
		l.setState(StateProbing, "пробный ход в read-only песочнице", time.Time{}, nil)
		if err := l.probe(ctx); err != nil {
			// The status file survives a failure on purpose: the reason the
			// daemon gave up is the one thing worth reading afterwards.
			l.setState(StateFailed, err.Error(), time.Time{}, nil)
			return err
		}
		l.logf("самопроверка пройдена: resume работает, поток событий разбирается")
	}

	for {
		if err := ctx.Err(); err != nil {
			l.stop()
			return nil
		}
		wait := l.step(ctx)
		if wait <= 0 {
			continue
		}
		if !sleepCtx(ctx, wait) {
			l.stop()
			return nil
		}
	}
}

// stop records a clean shutdown and takes the status file away: a stopped
// daemon must not leave a "running" behind for a tray icon to believe.
func (l *Loop) stop() {
	l.setState(StateStopped, "", time.Time{}, nil)
	l.status.Remove()
}

// Status returns the daemon's current state. Safe from any goroutine.
func (l *Loop) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current
}

// step does one unit of work and reports how long to wait before the next one.
func (l *Loop) step(ctx context.Context) time.Duration {
	sessions, err := codex.Scan(l.Dir)
	if err != nil {
		l.setState(StateFailed, "каталог сессий не прочитан: "+err.Error(), time.Time{}, nil)
		l.bump(func(s *Status) { s.Errors++ })
		return l.Tick
	}

	plan := Build(sessions, l.Policy, time.Now(), l.own)

	// Yielding comes first. The usage limit belongs to the account, so a turn
	// taken now is a turn taken away from the person at the keyboard.
	if plan.Interrupt {
		l.setState(StateYielding, "", plan.YieldUntil, nil)
		return l.Tick
	}

	if plan.Limited && plan.StartAt.After(time.Now()) {
		l.setState(StateLimit, "", plan.StartAt, nil)
		// Waited in short hops against the wall clock rather than one long
		// sleep: a laptop that suspends for two hours must wake up knowing the
		// window has opened, not still counting down.
		return l.Tick
	}

	runnable := plan.Runnable()
	if len(runnable) == 0 {
		l.setState(StateIdle, "возобновляемых целей нет", time.Time{}, nil)
		return l.Tick
	}

	goal := runnable[0].Session
	l.setState(StateRunning, "", time.Time{}, &goal)

	res, err := RunOnce(ctx, l.Policy, goal, Options{Trace: l.Log, Timeout: 60 * time.Minute})
	if err != nil {
		l.bump(func(s *Status) { s.Errors++ })
		l.setState(StateFailed, err.Error(), time.Time{}, &goal)
		return l.Tick
	}
	l.bump(func(s *Status) { s.Turns++ })
	l.own[goal.ID] = time.Now()

	switch {
	case res.Failure == FailOutOfCredits:
		// Not a wait. No window opens on its own here, so sitting in the loop
		// would mean sitting forever; a person has to act.
		l.bump(func(s *Status) { s.Errors++ })
		msg := "кредиты кончились — ожидание не поможет"
		if len(res.Errors) > 0 {
			msg = res.Errors[0]
		}
		l.setState(StateFailed, msg, time.Time{}, &goal)
		return l.Tick

	case res.UsageLimited:
		l.bump(func(s *Status) { s.Limits++ })
		until := res.ResetsAt
		if until.IsZero() {
			until = codex.NextReset(sessions)
		}
		l.setState(StateLimit, "упёрлись в лимит", until, &goal)
		return l.Tick

	case res.ExitCode != 0:
		l.bump(func(s *Status) { s.Errors++ })
		msg := fmt.Sprintf("ход завершился с кодом %d", res.ExitCode)
		if len(res.Errors) > 0 {
			msg += ": " + res.Errors[0]
		}
		l.setState(StateFailed, msg, time.Time{}, &goal)
		return l.Tick
	}

	l.logf("ход закончен за %s, событий %d", res.Duration.Round(time.Second), res.Parsed)
	// Straight on to the next examination: goals move one turn at a time, and
	// the freshest one is picked again from a rescan rather than assumed.
	return 0
}

// probe runs a single read-only turn to prove the machinery works.
func (l *Loop) probe(ctx context.Context) error {
	sessions, err := codex.Scan(l.Dir)
	if err != nil {
		return fmt.Errorf("каталог сессий не прочитан: %w", err)
	}
	goal, ok := Default(Goals(sessions), func(s codex.Session) bool {
		return dirExists(s.Cwd)
	})
	if !ok {
		return fmt.Errorf("нет ни одной цели, на которой можно проверить связку")
	}

	l.logf("проверяю на цели: %s", goal.Label())
	res, err := RunOnce(ctx, l.Policy, goal, Options{
		ReadOnly: true, Trace: l.Log, Timeout: 15 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("не удалось запустить codex: %w", err)
	}
	switch {
	case res.UsageLimited:
		// Not a failure: the machinery worked well enough to be told no.
		l.logf("самопроверка упёрлась в лимит — связка исправна")
		return nil
	case res.Lines == 0:
		return fmt.Errorf("codex не выдал ни строки (код выхода %d); запустите crescent -doctor", res.ExitCode)
	case res.Parsed == 0:
		return fmt.Errorf("ни одна из %d строк не разобралась как JSON: формат событий изменился, "+
			"сохраните его через crescent -run-once -read-only -raw /tmp/stream.jsonl", res.Lines)
	case res.ExitCode != 0 && len(res.Errors) > 0:
		return fmt.Errorf("пробный ход не прошёл: %s", res.Errors[0])
	}
	return nil
}

// sleepCtx waits, reporting false if the context was cancelled meanwhile.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
