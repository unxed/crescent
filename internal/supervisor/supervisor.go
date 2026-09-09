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
	StatePaused     State = "на паузе"
	// StateUnknownLimit is not the same as waiting for a reset: we do not know
	// whether work is possible, usually because the limits request could not
	// reach the backend. Saying "waiting for the reset" there was a lie.
	StateUnknownLimit State = "не удалось узнать лимит"
	// StateFailing: the loop is running but every attempt fails. Showing green
	// here was the worst thing the window did — nothing worked and the lamp
	// said everything was fine.
	StateFailing State = "ошибки при запуске"
)

// Lamp is the traffic light: the one thing a tired person should be able to
// read across the room without parsing a sentence.
type Lamp uint8

// Lamp values.
const (
	LampRed    Lamp = iota // stopped: nothing will happen until you act
	LampYellow             // waiting: on the limit, on the app, on the network
	LampGreen              // working
)

// Symbol renders the lamp. Coloured circles, because the toolkit's labels carry
// no colour of their own and this reads correctly everywhere.
func (l Lamp) Symbol() string {
	switch l {
	case LampGreen:
		return "🟢"
	case LampYellow:
		return "🟡"
	default:
		return "🔴"
	}
}

// Word is the state in one shouted word, next to the lamp.
func (l Lamp) Word() string {
	switch l {
	case LampGreen:
		return "РАБОТАЕТ"
	case LampYellow:
		return "ЖДЁТ"
	default:
		return "СТОИТ"
	}
}

// Lamp maps a status to the traffic light.
func (s Status) Lamp() Lamp {
	switch s.State {
	case StateWorking:
		return LampGreen
	case StateFailing:
		return LampRed
	case StateLimited, StateWaitingApp, StateUnknownLimit:
		return LampYellow
	default:
		return LampRed
	}
}

// Headline is the whole state in one line: lamp, word, and why.
func (s Status) Headline() string {
	l := s.Lamp()
	return l.Symbol() + "  " + l.Word() + " — " + s.Line()
}

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
			return fmt.Sprintf("лимит аккаунта исчерпан, сброс в %s",
				s.Until.Local().Format("15:04"))
		}
		return "лимит аккаунта исчерпан, время сброса сервер не назвал"
	case StateUnknownLimit:
		return "не удаётся спросить лимит у Codex — проверьте сеть; пробую снова"
	case StatePaused:
		return "пауза — цели не перезапускаются, пока не нажмёте «Продолжить»"
	case StateNoGoals:
		return "ни одна цель не отмечена галочкой"
	case StateWaitingApp:
		if s.Message != "" {
			return s.Message
		}
		return "ждёт, пока вы закроете приложение ChatGPT"
	case StateFailing:
		if s.Message != "" {
			return "не удаётся запустить: " + s.Message
		}
		return "не удаётся запустить цели — смотрите журнал"
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
	wake     chan struct{}
	paused   bool
	// fails counts consecutive failed restarts, so the lamp can stop claiming
	// success while nothing is getting through.
	fails    int
	lastFail string
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
		wake:     make(chan struct{}, 1),
		status:   Status{State: StateIdle, Since: time.Now()},
	}
}

// SetPaused stops or resumes the loop without tearing it down, so the pinned
// set and the connection survive a pause.
func (s *Supervisor) SetPaused(p bool) {
	s.mu.Lock()
	s.paused = p
	s.mu.Unlock()
	s.Wake()
}

// Paused reports whether the loop is standing down by request.
func (s *Supervisor) Paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// Wake makes the loop take a pass immediately instead of waiting out the poll
// interval — used the moment a goal is pinned, so the effect is visible at once
// rather than up to a poll later.
func (s *Supervisor) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
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

		if s.Paused() {
			s.set(StatePaused, "", time.Time{})
			if !s.sleep(ctx) {
				return nil
			}
			continue
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
			// Unknown is not permission — but it is also not a reset to wait
			// for, and saying so was misleading.
			s.set(StateUnknownLimit, "", time.Time{})
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

		// After the pass, tell the truth about it: if nothing got through, the
		// lamp must not stay green.
		s.mu.Lock()
		fails, why := s.fails, s.lastFail
		s.mu.Unlock()
		if fails > 0 {
			s.set(StateFailing, why, time.Time{})
		}

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
			s.mu.Lock()
			s.fails++
			s.lastFail = err.Error()
			s.mu.Unlock()
			continue
		}
		s.started[id] = time.Now()
		s.mu.Lock()
		s.fails, s.lastFail = 0, ""
		s.mu.Unlock()
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
		case <-s.wake:
			return true // woken early — re-examine now
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
	Updated  int64
}

// Goals returns everything the server offers, marked with whether it is pinned,
// so a window can show the full list with checkboxes.
func (s *Supervisor) Goals(ctx context.Context) ([]GoalView, error) {
	cands, err := s.client.Candidates(ctx)
	if err != nil {
		return nil, err
	}
	// Forked threads keep the name and the objective of the thread they came
	// from, so the same goal shows up several times — "f4 SDL в Alpine" four
	// times over. Only the freshest of each group can be driven usefully, and
	// showing the rest is a list the user has to disambiguate for no reason.
	best := map[string]appserver.Candidate{}
	var order []string
	for _, c := range cands {
		key := c.Thread.Label() + "\x00" + c.Goal.Objective
		prev, seen := best[key]
		if !seen {
			order = append(order, key)
			best[key] = c
			continue
		}
		if c.Thread.Updated > prev.Thread.Updated {
			best[key] = c
		}
	}

	out := make([]GoalView, 0, len(order))
	for _, key := range order {
		c := best[key]
		out = append(out, GoalView{
			ThreadID: c.Thread.ID,
			Label:    c.Thread.Label(),
			Status:   c.Goal.Status,
			Cwd:      c.Thread.Cwd,
			Updated:  c.Thread.Updated,
		})
	}
	// Freshest first: what was touched last is what the user is thinking about.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out, nil
}
