package supervisor

import (
	"sort"
	"testing"
	"time"

	"github.com/unxed/crescent/internal/appserver"
)

// dedupe mirrors what Goals does, so the rule can be checked without a server.
// Extracted here because the rule itself is what matters: forks carry the name
// and objective of the thread they came from, so the same goal appeared four
// times and pushed the goals that mattered off the visible list.
func dedupe(cands []appserver.Candidate) []GoalView {
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
	for _, k := range order {
		c := best[k]
		out = append(out, GoalView{ThreadID: c.Thread.ID, Label: c.Thread.Label(),
			Status: c.Goal.Status, Updated: c.Thread.Updated})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out
}

func cand(id, name, objective string, updated int64) appserver.Candidate {
	return appserver.Candidate{
		Thread: appserver.Thread{ID: id, Name: name, Updated: updated},
		Goal:   appserver.Goal{Objective: objective, Status: "paused"},
	}
}

// Four forks of one goal must collapse to one row, and the survivor must be the
// freshest — driving a stale fork would spend the quota on a dead branch.
func TestForksCollapseToTheFreshest(t *testing.T) {
	got := dedupe([]appserver.Candidate{
		cand("a", "f4 SDL в Alpine", "Обновись из репозитория", 100),
		cand("b", "f4 SDL в Alpine", "Обновись из репозитория", 300),
		cand("c", "f4 SDL в Alpine", "Обновись из репозитория", 200),
		cand("d", "Konsole", "довести Konsole", 400),
	})
	if len(got) != 2 {
		t.Fatalf("осталось %d строк, ожидалось 2: %+v", len(got), got)
	}
	for _, g := range got {
		if g.Label == "f4 SDL в Alpine" && g.ThreadID != "b" {
			t.Errorf("выжил не самый свежий форк: %s", g.ThreadID)
		}
	}
}

// Same name, different objective, is a different goal and must not be merged:
// collapsing it would silently drop work the user pinned.
func TestSameNameDifferentGoalSurvives(t *testing.T) {
	got := dedupe([]appserver.Candidate{
		cand("a", "Следовать инструкции", "Ты — Лунобот-1", 100),
		cand("b", "Следовать инструкции", "Ты — Лунобот-2", 200),
	})
	if len(got) != 2 {
		t.Fatalf("две разные цели схлопнулись в %d", len(got))
	}
}

// Freshest first: the goal touched last is the one being thought about, and
// burying it below stale ones is how the Луноботы ended up out of sight.
func TestFreshestGoalComesFirst(t *testing.T) {
	got := dedupe([]appserver.Candidate{
		cand("old", "Старая", "цель", 100),
		cand("new", "Свежая", "цель", 900),
	})
	if got[0].ThreadID != "new" {
		t.Errorf("сверху %q, ожидалась самая свежая", got[0].Label)
	}
}

// A goal can sit in status "active" while nothing runs: an interrupted or dead
// turn leaves the state behind and Codex does not start it again. Skipping
// every active goal meant crescent looked straight at the situation it exists
// to fix and decided there was nothing to do — a live journal went silent for
// hours under "все цели идут сами".
func TestStuckActiveGoalIsNotConsideredMoving(t *testing.T) {
	s := &Supervisor{
		active:    map[string]time.Time{},
		spend:     map[string]spendMark{},
		samples:   map[string]int{"никогда-ничего-не-делала": 2, "давно-молчит": 2},
		startedAt: time.Now().Add(-2 * ProgressMaxAge),
	}

	if s.isMoving("никогда-ничего-не-делала") {
		t.Error("цель без единого признака жизни считается работающей")
	}

	s.active["с-событиями"] = time.Now()
	if !s.isMoving("с-событиями") {
		t.Error("цель со свежим событием не считается работающей")
	}

	s.spend["с-токенами"] = spendMark{Tokens: 100, Moved: time.Now()}
	if !s.isMoving("с-токенами") {
		t.Error("цель со свежим расходом не считается работающей")
	}

	// Signs that have gone stale prove nothing.
	s.active["давно-молчит"] = time.Now().Add(-2 * ProgressMaxAge)
	s.spend["давно-молчит"] = spendMark{Tokens: 100, Moved: time.Now().Add(-2 * ProgressMaxAge)}
	if s.isMoving("давно-молчит") {
		t.Error("устаревшие признаки жизни всё ещё считаются работой")
	}
}

// Right after startup nothing has been observed yet, and a goal genuinely
// running elsewhere must not be restarted the instant the window opens.
// The benefit of the doubt now lasts as long as it takes to earn an answer: one
// reading of the usage counter tells nothing, two tell whether it moved. Waiting
// a fixed five minutes meant a goal that had already stopped sat untouched for
// five minutes at every start.
func TestBenefitOfTheDoubtLastsTwoSamples(t *testing.T) {
	s := &Supervisor{
		active:    map[string]time.Time{},
		spend:     map[string]spendMark{},
		samples:   map[string]int{},
		startedAt: time.Now().Add(-time.Hour), // возраст запуска больше ни на что не влияет
	}

	if !s.isMoving("цель") {
		t.Error("до первого замера цель уже объявлена остановившейся")
	}
	s.samples["цель"] = 1
	if !s.isMoving("цель") {
		t.Error("одного замера хватило для вывода — сравнивать было не с чем")
	}
	s.samples["цель"] = 2
	if s.isMoving("цель") {
		t.Error("после двух замеров без роста цель всё ещё считается работающей")
	}
}

// Whether a goal is waiting on a person is Codex's verdict — status blocked —
// not a reading of its messages. Deciding from the text was fragile both ways:
// a technical message about «разрешение экрана» read as a request, and a real
// block phrased in words the markers did not know read as nothing.
func TestWaitingIsDecidedByStatusNotByText(t *testing.T) {
	s := &Supervisor{
		statuses:  map[string]string{"stuck": "blocked", "busy": "active"},
		lastMsg:   map[string]string{"stuck": "Резолюция окна: разрешение экрана 1920x1080.", "busy": "Для возобновления напишите: «да»"},
		granted:   map[string]string{},
		started:   map[string]time.Time{},
		answering: map[string]bool{},
	}

	if w, _ := s.AwaitingWord("busy"); w {
		t.Error("активная цель объявлена ожидающей из-за текста сообщения")
	}
	w, phrase := s.AwaitingWord("stuck")
	if !w {
		t.Fatal("заблокированная цель не считается ожидающей")
	}
	if phrase != "" {
		t.Errorf("из технического текста извлечена фраза %q", phrase)
	}
	// No quoted phrase → the general grant goes out.
	if text, general := ReplyFor(phrase); !general || text == "" {
		t.Error("без фразы должно уходить общее разрешение")
	}
}

// One reply per message. Answering the same block twice cannot help; a second
// reply means the goal said something new.
func TestSameBlockIsAnsweredOnce(t *testing.T) {
	s := &Supervisor{
		statuses:  map[string]string{"g": "blocked"},
		lastMsg:   map[string]string{"g": "Цель заблокирована."},
		granted:   map[string]string{"g": "Цель заблокирована."}, // already answered
		started:   map[string]time.Time{},
		answering: map[string]bool{},
	}
	if w, _ := s.AwaitingWord("g"); w {
		t.Error("на уже отвеченное сообщение собираемся ответить снова")
	}
	s.lastMsg["g"] = "Теперь нужно другое: напишите «продолжай»"
	if w, phrase := s.AwaitingWord("g"); !w || phrase != "продолжай" {
		t.Errorf("новое сообщение не распознано как новый запрос: %v %q", w, phrase)
	}
}

// thread/goal/get lags the thread: a goal that answered a block and went back
// to work still reads as blocked there, while the event stream already says
// active. The line a person reads should carry the fresher word.
func TestLiveThreadStatusIsShownOverTheGoalRecord(t *testing.T) {
	s := &Supervisor{live: map[string]liveStatus{}}

	// Nothing from the stream yet: the goal record is all there is.
	if got := s.shownStatus("g", "blocked", false); got != "blocked" {
		t.Errorf("без свежих данных показан %q", got)
	}

	s.NoteThreadStatus("g", "active")
	if got := s.shownStatus("g", "blocked", false); got != "active" {
		t.Errorf("показан устаревший статус %q вместо свежего active", got)
	}

	// Not every goal emits a status event: activity alone proves it is active.
	if got := s.shownStatus("g2", "blocked", true); got != "active" {
		t.Errorf("цель с идущими событиями показана как %q", got)
	}

	// A stale stream word gives way back to the record.
	s.live["g"] = liveStatus{Status: "active", At: time.Now().Add(-2 * ProgressMaxAge)}
	if got := s.shownStatus("g", "blocked", false); got != "blocked" {
		t.Errorf("устаревшее сообщение потока всё ещё в ходу: %q", got)
	}
}

// Tokens spent while crescent watched, counted by accumulating each observed
// increase. Subtracting a start baseline from a running total looked equivalent
// and was not: a goal missing from one pass, or a counter reset by a restart,
// made the difference come out zero while the account was plainly being spent.
func TestSpendAccumulatesAcrossRestarts(t *testing.T) {
	s := &Supervisor{
		spend:   map[string]spendMark{},
		samples: map[string]int{},
		active:  map[string]time.Time{},
	}
	obs := func(id string, tokens int64) {
		s.observeSpend(id, id, appserver.Goal{TokensUsed: tokens, Status: "active"})
	}

	obs("a", 1000) // first sighting: remembered, not counted
	obs("a", 1500) // +500
	obs("b", 200)  // another goal appears
	obs("b", 700)  // +500
	if s.spentHere != 1000 {
		t.Errorf("накоплено %d, ожидалось 1000", s.spentHere)
	}

	// A goal that disappears for a pass must not subtract anything.
	obs("a", 1500)
	if s.spentHere != 1000 {
		t.Errorf("без прироста счёт изменился: %d", s.spentHere)
	}

	// A counter that goes backwards after a restart is not negative spending.
	obs("a", 100)
	if s.spentHere != 1000 {
		t.Errorf("сброс счётчика цели уменьшил общий расход: %d", s.spentHere)
	}
}
