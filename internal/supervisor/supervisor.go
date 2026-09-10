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
	// PerGoal is what each pinned goal has spent and when it last stirred, so
	// the window can show it next to the goal instead of one opaque total.
	PerGoal map[string]GoalSpend
	// NextPass is when the loop will look again. Every wait in this program
	// used to be invisible: the window said nothing while five minutes of grace
	// period elapsed, and a person watching had no way to tell waiting from
	// broken.
	NextPass time.Time

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
	// broken holds goals Codex cannot write history for, with the reason. No
	// number of restarts fixes that, so crescent stops trying and says so.
	broken map[string]string
	// noisy remembers goals whose history discrepancy has already been
	// mentioned, so it is said once instead of hundreds of times.
	noisy map[string]bool
	// lastMsg is each goal's most recent message. A goal blocked in plain
	// speech names in it the words it wants to hear back, and there is nowhere
	// else to read them.
	lastMsg map[string]string
	// answering holds goals whose confirmation is being sent right now. Without
	// it, several passes read "waiting" before the first send completed and the
	// same phrase went out three times in one second.
	answering map[string]bool
	// trace, when set, receives one line per decision. Every pass so far left
	// no record of what it examined or why it did nothing, so a loop that
	// skipped every goal was indistinguishable from a loop that never ran.
	trace func(string)
	// samples counts how many times each goal's usage has been read. Two
	// readings are the least that can show movement; until then a goal is
	// neither proved working nor proved stopped.
	samples map[string]int
	// startedAt is when watching began, used to give goals one grace period
	// before a stuck "active" is judged stuck.
	startedAt time.Time
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
		client:    o.Client,
		pins:      o.Pins,
		jour:      o.Journal,
		prompt:    prompt,
		poll:      poll,
		started:   map[string]time.Time{},
		turns:     map[string]string{},
		spend:     map[string]spendMark{},
		active:    map[string]time.Time{},
		samples:   map[string]int{},
		onChange:  o.OnChange,
		wake:      make(chan struct{}, 1),
		startedAt: time.Now(),
		broken:    map[string]string{},
		noisy:     map[string]bool{},
		lastMsg:   map[string]string{},
		answering: map[string]bool{},
		status:    Status{State: StateIdle, Since: time.Now()},
	}
}

// NoteServerLine takes one line of app-server logging and records anything
// worth a person's attention.
//
// It used to mark a goal unfixable on seeing "expected ordinal N, got N-1" and
// stop restarting it. That was wrong: a probe run showed the model working
// normally — 145 000 tokens and 293 message fragments in five minutes — while
// those very lines were being logged. Codex notices the discrepancy itself,
// falls back, and carries on. The line is noise; treating it as a verdict
// silenced goals that were working.
func (s *Supervisor) NoteServerLine(line string) {
	if !appserver.IsHistoryDesynced(line) {
		return
	}
	id := appserver.ThreadIDIn(line)
	if id == "" {
		return
	}
	s.mu.Lock()
	known := s.noisy[id]
	s.noisy[id] = true
	s.mu.Unlock()

	// Said once per goal, as information, with no consequence for restarts.
	if !known {
		s.note2(id, "Codex сообщает о расхождении истории треда и обходит его сам; "+
			"на работу это не влияет")
	}
}

// startedFor is when this goal was last pushed, for the trace.
func (s *Supervisor) startedFor(id string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started[id]
}

func sinceOrNever(t time.Time) string {
	if t.IsZero() {
		return "никогда"
	}
	return time.Since(t).Round(time.Second).String() + " назад"
}

// SetTrace turns on the decision trace.
func (s *Supervisor) SetTrace(fn func(string)) {
	s.mu.Lock()
	s.trace = fn
	s.mu.Unlock()
}

func (s *Supervisor) tracef(format string, a ...any) {
	s.mu.Lock()
	fn := s.trace
	s.mu.Unlock()
	if fn != nil {
		fn(fmt.Sprintf(format, a...))
	}
}

// Broken reports why a goal cannot run, if it cannot.
func (s *Supervisor) Broken(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.broken[id]
}

// NoteMessage remembers what a goal last said.
func (s *Supervisor) NoteMessage(threadID, text string) {
	if threadID == "" || strings.TrimSpace(text) == "" {
		return
	}
	s.mu.Lock()
	s.lastMsg[threadID] = text
	s.mu.Unlock()
}

// AwaitingWord reports a goal stopped in plain speech, together with the words
// it asked for. The phrase is empty when the goal is waiting but did not quote
// one — then only a person can decide what to say.
// tailOf returns the end of a message: what a goal asks for is said last.
func tailOf(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

// FetchLastMessage asks the server what a goal said last, for goals that
// blocked before this run began.
func (s *Supervisor) FetchLastMessage(ctx context.Context, id string) {
	s.mu.Lock()
	_, have := s.lastMsg[id]
	s.mu.Unlock()
	if have {
		return
	}
	msgs, err := s.client.RecentAgentMessages(ctx, id, 200)
	if err != nil {
		// Said once per goal: swallowing this is what hid a whole round of
		// investigation, because a request that failed looked exactly like a
		// goal that had nothing to say.
		s.mu.Lock()
		known := s.noisy["fetch:"+id]
		s.noisy["fetch:"+id] = true
		s.mu.Unlock()
		if !known {
			s.note2(id, "не удалось прочитать последнее сообщение цели: "+err.Error())
		}
		return
	}
	// The newest message that actually asks for something. Our own restart
	// makes the model reply, so the newest message is usually that reply and
	// the request is a few messages behind it.
	msg := msgs[0]
	for _, m := range msgs {
		if appserver.AsksForConfirmation(m) {
			msg = m
			break
		}
	}
	s.NoteMessage(id, msg)

	// What the message actually says, when nothing actionable was found in it.
	// Reporting only that the read succeeded left the next question — why no
	// phrase — as unanswerable as the one before it.
	switch {
	case !appserver.AsksForConfirmation(msg):
		s.note2(id, fmt.Sprintf(
			"просмотрено %d сообщений цели, просьбы подтвердить ни в одном нет; "+
				"последние слова: %s", len(msgs), tailOf(msg, 250)))
	case appserver.UnblockPhrase(msg) == "":
		s.note2(id, "цель ждёт ответа, но не назвала фразу в кавычках; "+
			"последние слова: "+tailOf(msg, 300))
	default:
		s.note2(id, "цель просит ответить: "+appserver.UnblockPhrase(msg))
	}
}

func (s *Supervisor) AwaitingWord(id string) (waiting bool, phrase string) {
	s.mu.Lock()
	msg := s.lastMsg[id]
	defer s.mu.Unlock()
	if msg == "" || !appserver.AsksForConfirmation(msg) {
		return false, ""
	}
	// A goal just answered is not waiting: without this the same phrase would
	// be sent again on every pass until the goal's next message arrived.
	if s.answering[id] {
		return false, "" // a confirmation is already on its way
	}
	if last := s.started[id]; !last.IsZero() && time.Since(last) < 2*time.Minute {
		return false, ""
	}
	return true, appserver.UnblockPhrase(msg)
}

// SendWord replies to a goal in its own thread, which is how a block stated in
// plain speech is lifted.
func (s *Supervisor) SendWord(ctx context.Context, id, text string) error {
	s.mu.Lock()
	if s.answering[id] {
		s.mu.Unlock()
		return nil // already going out
	}
	s.answering[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.answering, id)
		s.mu.Unlock()
	}()

	if err := s.client.Resume(ctx, id); err != nil {
		return err
	}
	if err := s.client.StartTurn(ctx, id, text); err != nil {
		return err
	}
	s.mu.Lock()
	s.started[id] = time.Now()
	delete(s.lastMsg, id) // asked and answered
	s.mu.Unlock()
	s.bumpRestart()
	s.note2(id, "отправлено подтверждение: "+text)
	return nil
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

	// A pass counter in the trace: silence then means the loop stopped, not
	// that it looked and found nothing to do. Those two were indistinguishable
	// in every log so far.
	pass := 0
	for {
		pass++
		s.tracef("=== проход %d, %s ===", pass, time.Now().Format("15:04:05"))
		if ctx.Err() != nil {
			s.note("цикл наблюдения остановлен: " + ctx.Err().Error())
			s.set(StateIdle, "остановлено", time.Time{})
			return nil
		}

		if s.Paused() {
			s.tracef("--- проход: пауза ---")
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
			s.tracef("--- проход: нет закреплённых целей ---")
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
			s.tracef("--- проход: лимит исчерпан, ждём до %s ---", at.Local().Format("15:04:05"))
			s.set(StateLimited, "", at)
			if !s.sleep(ctx) {
				return nil
			}
			continue
		}

		s.set(StateWorking, "", time.Time{})
		s.tracef("--- проход: лимит позволяет, целей закреплено %d ---", s.pins.Count())
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
	s.samples[id]++
	now := time.Now()
	grew := seen && goal.TokensUsed > prev.Tokens
	switch {
	case grew:
		s.spend[id] = spendMark{Tokens: goal.TokensUsed, Seconds: goal.TimeUsedSeconds, Moved: now}
	case !seen:
		// First sighting: record the counter, but leave Moved unset. Stamping
		// it with the current time made the very act of looking count as
		// movement, and the goal then read as working for the whole freshness
		// window without a single token having been spent.
		s.spend[id] = spendMark{Tokens: goal.TokensUsed, Seconds: goal.TimeUsedSeconds}
	default:
		s.spend[id] = spendMark{Tokens: goal.TokensUsed, Seconds: goal.TimeUsedSeconds, Moved: prev.Moved}
	}
	last := s.spend[id].Moved
	s.mu.Unlock()

	if grew {
		return true, fmt.Sprintf("%s: +%d токенов (всего %d)",
			label, goal.TokensUsed-prev.Tokens, goal.TokensUsed)
	}
	// Only when there is a moment to measure from. Reporting time since the
	// zero value printed "токены не тратятся 2562047h47m0s", which is the age
	// of the universe according to Go.
	if seen && !last.IsZero() && now.Sub(last) > ProgressMaxAge {
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

	s.proveMovement(ctx)
}

// waitReason explains, in the goal's own terms, what it is waiting for and
// until when. This is what the window shows: a countdown beats silence, and a
// named wait beats a countdown.
func (s *Supervisor) waitReason(id string, goal appserver.Goal) (string, time.Time) {
	s.mu.Lock()
	started := s.started[id]
	last := s.active[id]
	mark, hasSpend := s.spend[id]
	broken := s.broken[id]
	s.mu.Unlock()

	switch {
	case broken != "":
		return broken, time.Time{}
	case !appserver.Restartable(goal.Status):
		return "ждёт вас: статус " + goal.Status, time.Time{}
	case !started.IsZero() && time.Since(started) < 2*time.Minute:
		return "запущена, жду появления хода", started.Add(2 * time.Minute)
	}

	// The grace period: right after startup nothing has been observed yet, so a
	// goal is given the benefit of the doubt. It was the single most confusing
	// wait in the program, because nothing said it was happening.
	s.mu.Lock()
	n := s.samples[id]
	s.mu.Unlock()
	if last.IsZero() && n < 2 {
		return "смотрю, растёт ли расход (замер 1 из 2)", time.Time{}
	}
	if !last.IsZero() && time.Since(last) < ProgressMaxAge {
		return "работает: событие " + time.Since(last).Round(time.Second).String() + " назад", time.Time{}
	}
	if hasSpend && !mark.Moved.IsZero() && time.Since(mark.Moved) < ProgressMaxAge {
		return "работает: расход растёт", time.Time{}
	}
	return "", time.Time{}
}

// isMoving reports whether a goal has shown any sign of life recently.
//
// A goal can sit in status "active" while nothing runs: a turn that died or was
// interrupted leaves the state behind, and Codex does not start it again.
// Skipping every active goal meant crescent looked at exactly the situation it
// exists to fix and decided there was nothing to do — the log went silent for
// hours with "все цели идут сами". Status says what Codex believes; spend and
// events say what is happening.
func (s *Supervisor) isMoving(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if last := s.active[id]; !last.IsZero() && time.Since(last) < ProgressMaxAge {
		return true
	}
	if m, ok := s.spend[id]; ok && !m.Moved.IsZero() && time.Since(m.Moved) < ProgressMaxAge {
		return true
	}
	// Nothing observed yet. The benefit of the doubt lasts exactly as long as it
	// takes to earn an answer: one sample tells nothing, two tell whether the
	// count moved. Waiting a fixed five minutes meant a goal that had already
	// stopped sat untouched for five minutes at every start — the single thing
	// that made crescent look asleep.
	return s.samples[id] < 2
}

// proveMovement establishes whether any pinned goal is really working, and
// counts what has been spent. Shared by the ordinary pass and by the paused
// one, so both judge by the same evidence.
func (s *Supervisor) proveMovement(ctx context.Context) {
	running, moving := 0, 0
	var progress string
	perGoal := map[string]GoalSpend{}

	for _, id := range s.pins.IDs() {
		goal, err := s.client.Goal(ctx, id)
		if err != nil {
			s.forgetIfGone(id, err)
			continue
		}
		label := s.pins.Label(id)
		if strings.EqualFold(goal.Status, appserver.StatusActive) {
			running++
		}

		grew, note := s.observeSpend(id, label, goal)

		s.mu.Lock()
		lastSeen := s.active[id]
		s.mu.Unlock()
		streaming := !lastSeen.IsZero() && time.Since(lastSeen) < ProgressMaxAge

		sp := GoalSpend{Tokens: goal.TokensUsed, Seconds: goal.TimeUsedSeconds,
			Status: goal.Status, LastSeen: lastSeen, Spending: grew || streaming}
		if !appserver.Restartable(goal.Status) {
			// The words it is waiting for live in its last message, and a goal
			// blocked before this run began never said them where we could hear.
			s.FetchLastMessage(ctx, id)
		}
		sp.Waiting, sp.Until = s.waitReason(id, goal)
		perGoal[id] = sp

		switch {
		case grew:
			moving++
			progress = note
		case streaming:
			moving++
			if progress == "" {
				progress = fmt.Sprintf("%s: работает (событие %s назад)",
					label, time.Since(lastSeen).Round(time.Second))
			}
		case strings.EqualFold(goal.Status, appserver.StatusActive):
			// Active, but nothing is reaching us. The same account can be
			// driven from another machine, and then the goal is genuinely
			// running while this crescent has no part in it and no events to
			// show. Saying "работает" here would claim credit for work we
			// cannot see and cannot report.
			if progress == "" {
				progress = label + ": числится запущенной, но событий сюда не приходит"
			}
		}
		if note != "" && !grew && !streaming {
			s.note2(id, note)
		}
	}

	var total int64
	s.mu.Lock()
	s.status.Running = running
	s.status.PerGoal = perGoal
	for _, m := range s.spend {
		total += m.Tokens
	}
	if s.baseline == 0 && total > 0 {
		s.baseline = total
	}
	s.status.Spent, s.status.SpentSince = total, total-s.baseline
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
	// Movement is established by the survey, which asks for the evidence that
	// actually proves work: tokens spent or events in the stream. Proving it
	// here from the number of goals whose status says "active" was the weak
	// test all over again — and it lit the lamp green over an account where
	// nothing at all was happening, because a goal can be active on another
	// machine sharing the same account.
	defer s.proveMovement(ctx)

	for _, id := range s.pins.IDs() {
		if ctx.Err() != nil {
			return
		}
		label := s.pins.Label(id)
		s.jour.Label(id, label)

		goal, err := s.client.Goal(ctx, id)
		if err != nil {
			if s.forgetIfGone(id, err) {
				s.tracef("  %s: снята — треда нет", label)
				continue
			}
			s.tracef("  %s: ПРОПУСК — состояние не прочитано: %v", label, err)
			s.note2(id, "состояние не прочитано: "+err.Error())
			continue
		}
		if why := s.Broken(id); why != "" {
			s.tracef("  %s: ПРОПУСК — %s", label, why)
			continue
		}
		moving := s.isMoving(id)
		s.tracef("  %s: статус=%q объектив=%v движется=%v последний_запуск=%s",
			label, goal.Status, goal.Set(), moving, sinceOrNever(s.startedFor(id)))
		switch {
		case !goal.Set():
			s.tracef("  %s: ПРОПУСК — у треда нет цели", label)
			continue
		case !appserver.Restartable(goal.Status):
			s.tracef("  %s: ПРОПУСК — статус %q не перезапускается", label, goal.Status)
			continue
		case strings.EqualFold(goal.Status, appserver.StatusActive) && moving:
			s.tracef("  %s: ПРОПУСК — работает по-настоящему", label)
			continue
		case time.Since(s.started[id]) < 2*time.Minute:
			s.tracef("  %s: ПРОПУСК — запущена %s назад, ждём появления хода",
				label, time.Since(s.started[id]).Round(time.Second))
			continue
		}
		s.tracef("  %s: ЗАПУСКАЮ (resume + turn/start)", label)
		if err := s.client.Restart(ctx, id, s.prompt); err != nil {
			s.tracef("  %s: ЗАПУСК НЕ УДАЛСЯ: %v", label, err)
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
		why := "было " + goal.Status
		if strings.EqualFold(goal.Status, appserver.StatusActive) {
			why = "числилась запущенной, но не подавала признаков жизни"
		}
		s.note2(id, "перезапущено ("+why+")")
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
	s.mu.Lock()
	s.status.NextPass = time.Now().Add(s.poll)
	snap := s.status
	cb := s.onChange
	s.mu.Unlock()
	if cb != nil {
		cb(snap)
	}

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
// GoalSpend is one goal's own counters, straight from thread/goal/get.
type GoalSpend struct {
	Tokens   int64
	Seconds  int64
	Status   string
	LastSeen time.Time
	// Waiting says what this goal is waiting for, and Until when the wait ends.
	// Empty means it is not waiting on anything.
	Waiting string
	Until   time.Time
	// Spending is whether this goal's own usage grew since the last look.
	Spending bool
}

// Lamp is this goal's own traffic light.
func (g GoalSpend) Lamp() (Lamp, string) {
	return GoalLamp(g.Status, g.Waiting, g.Spending)
}

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
