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

// Lamp derives the traffic light from evidence gathered recently enough to
// trust. Nothing proved, or proof gone stale, means red.
func (s Status) Lamp() Lamp {
	lamp, _ := s.Evidence.Lamp(time.Now())
	return lamp
}

// Why explains the lamp in one phrase.
func (s Status) Why() string {
	_, why := s.Evidence.Lamp(time.Now())
	return why
}

// Headline is the whole state in one line: lamp, word, and why.
func (s Status) Headline() string {
	lamp, why := s.Evidence.Lamp(time.Now())
	return lamp.Symbol() + "  " + lamp.Word() + " — " + why
}

// Status is a snapshot for whoever is watching — a window, a tray, a terminal.
type Status struct {
	State   State
	Message string
	Until   time.Time // when a wait ends, if known
	Pinned  int
	// Running is how many pinned goals Codex is already advancing on its own.
	// Without it, "работает / перезапусков: 0" is indistinguishable from
	// broken — which is exactly how it looked.
	Running  int
	Restarts int
	Since    time.Time

	// Limits is every usage window the account has, ready to show. Asked for
	// from the start and never displayed until now.
	Limits []string
	// Spent is how many tokens the pinned goals have used, and how much of that
	// arrived since crescent started watching.
	Spent, SpentSince int64

	// Evidence is what has actually been proved about the work, and when. The
	// lamp is derived from it rather than stored, so it fails safe.
	Evidence Evidence
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
		return "пауза — работа остановлена, наблюдение продолжается"
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
		if s.Running > 0 && s.Running == s.Pinned {
			return fmt.Sprintf("все %d целей идут сами — перезапускать нечего", s.Pinned)
		}
		if s.Running > 0 {
			return fmt.Sprintf("идут сами: %d из %d; остальные жду перезапустить", s.Running, s.Pinned)
		}
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
	// turns remembers the turn in progress for each thread, learned from the
	// event stream. turn/interrupt cannot be issued without it.
	turns map[string]string
	// spend remembers each goal's token count and when it last changed, which
	// is the only evidence that a model is working rather than merely marked
	// active.
	spend map[string]spendMark
	// active is when each thread last produced anything at all in the event
	// stream, which proves work even when the token counter does not move.
	active map[string]time.Time
	// baseline is the token total when watching began, so the window can show
	// what was spent under crescent rather than the lifetime total.
	baseline int64
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
		turns:    map[string]string{},
		spend:    map[string]spendMark{},
		active:   map[string]time.Time{},
		onChange: o.OnChange,
		wake:     make(chan struct{}, 1),
		status:   Status{State: StateIdle, Since: time.Now()},
	}
}

// NoteActivity records that a thread produced something in the event stream.
//
// This is direct evidence of work, and it matters because tokensUsed is not
// always filled in: a live run showed a goal executing commands, visible in its
// own log, while the token count stayed at zero and the lamp therefore claimed
// nothing was being spent. Anything the model does — a command, a message, a
// turn event — proves it is working, whatever the counters say.
func (s *Supervisor) NoteActivity(threadID string) {
	if threadID == "" {
		return
	}
	s.mu.Lock()
	s.active[threadID] = time.Now()
	s.mu.Unlock()
}

// NoteTurn records the turn a thread is currently running, as seen in the event
// stream.
func (s *Supervisor) NoteTurn(threadID, turnID string) {
	if threadID == "" || turnID == "" {
		return
	}
	s.mu.Lock()
	s.turns[threadID] = turnID
	s.mu.Unlock()
}

// SetPaused stops or resumes the work.
//
// Pausing means stopping: turns in progress are interrupted, because a pause
// that leaves them running keeps spending tokens while the person believes they
// have stopped. Monitoring deliberately keeps going — the moment you have
// stopped the work is the moment you most want to see that it really stopped,
// and what the account limit is doing.
func (s *Supervisor) SetPaused(p bool) {
	s.mu.Lock()
	s.paused = p
	pinned := s.pins.IDs()
	turns := make(map[string]string, len(s.turns))
	for k, v := range s.turns {
		turns[k] = v
	}
	s.mu.Unlock()

	if p {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, id := range pinned {
			turn := turns[id]
			if turn == "" {
				continue // nothing running that we know of
			}
			if err := s.client.Interrupt(ctx, id, turn); err != nil {
				s.note2(id, "остановить ход не удалось: "+err.Error())
				continue
			}
			s.note2(id, "ход остановлен по паузе")
		}
	}
	s.Wake()
}

// Paused reports whether the loop is standing down by request.
func (s *Supervisor) Paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// prove records established facts. Each Fact carries its own timestamp, set by
// whoever established it — never by anyone else, or one live proof would keep
// the others alive without checking them.
func (s *Supervisor) prove(f func(*Evidence)) {
	s.mu.Lock()
	f(&s.status.Evidence)
	// Local truths are read straight from their source, so they are current by
	// construction and need no timestamp at all.
	s.status.Evidence.Paused = s.paused
	s.status.Evidence.Pinned = s.pins.Count() > 0
	snap := s.status
	cb := s.onChange
	s.mu.Unlock()
	if cb != nil {
		cb(snap)
	}
}

// now is a helper for stamping a fact at the moment it is established.
func fact(ok bool) Fact { return Fact{OK: ok, At: time.Now()} }

// heartbeat keeps the evidence fresh. It has to run faster than evidence
// expires, or a healthy system would flicker red between passes.
func (s *Supervisor) heartbeat(ctx context.Context) {
	tick := time.NewTicker(LivenessMaxAge / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			pingCtx, cancel := context.WithTimeout(ctx, LivenessMaxAge)
			err := s.client.Ping(pingCtx)
			cancel()
			// The heartbeat proves liveness and nothing else. It must not
			// touch any other fact's timestamp.
			s.prove(func(e *Evidence) { e.ServerAlive = fact(err == nil) })
		}
	}
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
	prevState, prevMsg := s.status.State, s.status.Message
	if s.status.State != state || s.status.Message != msg {
		s.status.Since = time.Now()
	}
	s.status.State, s.status.Message, s.status.Until = state, msg, until
	s.status.Pinned = s.pins.Count()
	snap := s.status
	cb := s.onChange
	changed := prevState != state || prevMsg != msg
	s.mu.Unlock()

	// Every state change goes into the journal. Without this the log carried
	// only pins and per-goal events, so a loop sitting in "waiting for the
	// limit" or "cannot read the limit" left no trace and looked from outside
	// like a program doing nothing at all.
	if changed && s.jour != nil {
		s.jour.Note("", "состояние: "+snap.Line())
	}
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
	go s.heartbeat(ctx)
	for {
		if ctx.Err() != nil {
			s.set(StateIdle, "остановлено", time.Time{})
			return nil
		}

		if s.Paused() {
			s.set(StatePaused, "", time.Time{})
			// Monitoring continues through a pause: the limit is still read and
			// the goals are still surveyed, so the window keeps telling the
			// truth about an account that is no longer being spent.
			s.survey(ctx)
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
		if err == nil {
			s.mu.Lock()
			s.status.Limits = limits.Summary()
			s.mu.Unlock()
		}
		s.prove(func(e *Evidence) {
			e.LimitChecked = fact(true)
			e.Online = fact(err == nil)
			if err == nil {
				limited, _ := limits.Exhausted()
				e.LimitOK = fact(!limited)
			} else {
				e.LimitOK = fact(false)
			}
		})
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

// spendMark is the last observed token count for a goal and when it moved.
type spendMark struct {
	Tokens  int64
	Seconds int64
	Moved   time.Time
}

// observeSpend records a goal's token count and reports whether it grew. Growth
// is the proof that work is happening: a model that is thinking is billing
// tokens while it thinks, so a goal that has spent nothing for minutes is not
// working, whatever its status says.
func (s *Supervisor) observeSpend(id, label string, goal appserver.Goal) (moving bool, note string) {
	s.mu.Lock()
	prev, seen := s.spend[id]
	now := time.Now()
	grew := seen && goal.TokensUsed > prev.Tokens
	if !seen || grew {
		s.spend[id] = spendMark{Tokens: goal.TokensUsed, Seconds: goal.TimeUsedSeconds, Moved: now}
	} else {
		s.spend[id] = spendMark{Tokens: goal.TokensUsed, Seconds: goal.TimeUsedSeconds, Moved: prev.Moved}
	}
	last := s.spend[id].Moved
	s.mu.Unlock()

	if grew {
		return true, fmt.Sprintf("%s: +%d токенов (всего %d)",
			label, goal.TokensUsed-prev.Tokens, goal.TokensUsed)
	}
	if seen && now.Sub(last) > ProgressMaxAge {
		return false, fmt.Sprintf("%s: токены не тратятся %s (статус %s)",
			label, now.Sub(last).Round(time.Minute), goal.Status)
	}
	return false, ""
}

// forgetIfGone unpins a goal whose thread no longer exists, and says so once.
//
// Without this a deleted chat stays pinned for ever: every pass asks about it,
// every pass fails identically, and the journal fills with the same line while
// the goal can never run again. Unpinning is the honest response — there is
// nothing left to watch.
func (s *Supervisor) forgetIfGone(id string, err error) bool {
	if !appserver.IsThreadGone(err) {
		return false
	}
	label := s.pins.Label(id)
	if label == "" {
		label = id
	}
	s.pins.Set(id, label, false)
	s.note("цель «" + label + "» снята: её чат больше не существует")
	return true
}

// survey refreshes what is known without starting anything: the account limit
// and whether the pinned goals are moving. Used while paused, so that stopping
// the work does not also stop the watching.
func (s *Supervisor) survey(ctx context.Context) {
	limits, err := s.client.RateLimitsWithRetry(ctx, 1)
	if err == nil {
		s.mu.Lock()
		s.status.Limits = limits.Summary()
		s.mu.Unlock()
	}
	s.prove(func(e *Evidence) {
		e.LimitChecked = fact(true)
		e.Online = fact(err == nil)
		if err == nil {
			limited, _ := limits.Exhausted()
			e.LimitOK = fact(!limited)
		} else {
			e.LimitOK = fact(false)
		}
	})

	running, moving := 0, 0
	var progress string
	for _, id := range s.pins.IDs() {
		goal, gerr := s.client.Goal(ctx, id)
		if gerr != nil {
			s.forgetIfGone(id, gerr)
			continue
		}
		if strings.EqualFold(goal.Status, appserver.StatusActive) {
			running++
		}
		grew, note := s.observeSpend(id, s.pins.Label(id), goal)

		// Either kind of evidence counts: tokens spent, or anything at all
		// coming out of the goal in the event stream.
		s.mu.Lock()
		lastSeen := s.active[id]
		s.mu.Unlock()
		streaming := !lastSeen.IsZero() && time.Since(lastSeen) < ProgressMaxAge

		if grew || streaming {
			moving++
			if grew {
				progress = note
			} else if progress == "" {
				progress = fmt.Sprintf("%s: работает (последнее событие %s назад)",
					s.pins.Label(id), time.Since(lastSeen).Round(time.Second))
			}
			note = "" // not idle after all; do not log the idle complaint
		}
		if note != "" {
			// Written to the goal's own log, so a goal that looked silent now
			// says what it is or is not doing.
			s.note2(id, note)
		}
	}
	var total, since int64
	s.mu.Lock()
	s.status.Running = running
	for _, m := range s.spend {
		total += m.Tokens
	}
	if s.baseline == 0 && total > 0 {
		s.baseline = total // first reading is the starting point, not spending
	}
	since = total - s.baseline
	s.status.Spent, s.status.SpentSince = total, since
	s.mu.Unlock()

	s.prove(func(e *Evidence) {
		e.GoalsMoving = fact(moving > 0)
		e.Progress = progress
	})
}

// restartPinned pushes each pinned goal that has stopped. Status is re-read
// every pass, never remembered: a goal may have finished, been paused by hand,
// or been taken over by the application between passes.
func (s *Supervisor) restartPinned(ctx context.Context) {
	running := 0
	defer func() {
		s.mu.Lock()
		s.status.Running = running
		s.mu.Unlock()
		// Movement is a fact about the goals, established this pass.
		s.prove(func(e *Evidence) { e.GoalsMoving = fact(running > 0) })
	}()

	for _, id := range s.pins.IDs() {
		if ctx.Err() != nil {
			return
		}
		label := s.pins.Label(id)
		s.jour.Label(id, label)

		goal, err := s.client.Goal(ctx, id)
		if err != nil {
			if s.forgetIfGone(id, err) {
				continue
			}
			s.note2(id, "состояние не прочитано: "+err.Error())
			continue
		}
		switch {
		case !goal.Set():
			continue
		case !appserver.Restartable(goal.Status):
			continue
		case strings.EqualFold(goal.Status, appserver.StatusActive):
			running++
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
	s.prove(func(e *Evidence) { e.WaitingApp = true })
	s.set(StateWaitingApp, "закройте "+name+" целиком", time.Time{})
	s.note("жду закрытия приложения " + name)
	for {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(3 * time.Second):
			if r, _ := codexcli.AppRunning(); !r {
				s.prove(func(e *Evidence) { e.WaitingApp = false })
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
