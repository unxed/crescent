package supervisor

import (
	"fmt"
	"strings"
	"time"
)

// How long each kind of proof stays good. A fact's life must exceed the
// interval at which it is refreshed, or a healthy system flickers red; and it
// must be short enough that a stopped refresher is noticed quickly.
const (
	// LivenessMaxAge covers the heartbeat, which runs every few seconds.
	LivenessMaxAge = 10 * time.Second
	// DefaultProgressMaxAge is how long a goal may go without spending a token
	// before crescent stops calling it working. A model that is thinking still
	// bills tokens as it goes, so silence this long means it is not thinking.
	DefaultProgressMaxAge = 5 * time.Minute
	// RetryBlockedAfter is how long to wait before answering a goal that stayed
	// blocked after the previous answer. Long enough not to spam a goal that is
	// simply slow, short enough that a standstill does not last hours.
	RetryBlockedAfter = 10 * time.Minute
	// SurveyMaxAge covers what the main pass establishes: the account limit and
	// whether goals are moving. The pass runs on the poll interval, so this is
	// three times that, leaving room for one missed round.
	SurveyMaxAge = 95 * time.Second
)

// ProgressMaxAge is the live value, adjustable for investigation: waiting five
// minutes to see one decision makes a debugging round unbearably slow, and the
// length of the grace period is itself a thing under examination.
var ProgressMaxAge = DefaultProgressMaxAge

// SetProgressMaxAge changes how long a goal may stay silent before it stops
// counting as working. Intended for investigation, not for normal running.
func SetProgressMaxAge(d time.Duration) {
	if d > 0 {
		ProgressMaxAge = d
	}
}

// Fact is one thing crescent has established, and when.
//
// Each fact carries its own timestamp, and this is the whole point. Sharing one
// timestamp across all facts meant the heartbeat — which proves only that
// app-server answers — kept refreshing the stamp for every other fact too, so
// "goals are moving" stayed valid long after anyone checked, and stayed valid
// through a pause. One healthy air line was holding the brakes off for the
// entire train.
type Fact struct {
	OK bool
	At time.Time
}

// Proved reports whether this fact is both true and recent enough to trust.
// The zero Fact is never proved: nothing established is not the same as
// established false, but both must keep the lamp off green.
func (f Fact) Proved(now time.Time, maxAge time.Duration) bool {
	return f.OK && !f.At.IsZero() && now.Sub(f.At) <= maxAge
}

// Stale reports a fact that was once true but has not been refreshed. This is
// the dangerous case — the collector stopped — and it is reported apart from a
// fact that is simply false.
func (f Fact) Stale(now time.Time, maxAge time.Duration) bool {
	return !f.At.IsZero() && now.Sub(f.At) > maxAge
}

// Evidence is everything crescent has established about the work.
//
// The lamp is derived from this and never stored. The complete table of states,
// checked in this order, with the lamp each produces:
//
//	условие                                      лампа   почему
//	───────────────────────────────────────────────────────────────────────
//	ничего ещё не установлено                    🔴      нет оснований
//	сведения о сервере устарели                  🔴      сборщик молчит
//	сервер не отвечает                           🔴      связи нет
//	наблюдение на паузе                          🔴      работа остановлена
//	ждём закрытия приложения                     🔴      работать нельзя
//	ни одна цель не отмечена                     🟡      нечего вести
//	лимит ещё не спрашивали                      🟡      неизвестно
//	сведения о лимите устарели                   🟡      проверка отстала
//	нет связи с OpenAI                           🟡      спросить не смогли
//	лимит исчерпан                               🟡      ждём сброса
//	сведения о движении целей устарели           🟡      обход отстал
//	ни одна отмеченная цель не движется          🟡      работы нет
//	всё выше установлено и свежо                 🟢      работа идёт
//
// Green is reachable only through the bottom row, which requires every fact
// above to be individually true and individually fresh.
type Evidence struct {
	// Liveness, refreshed by the heartbeat.
	ServerAlive Fact

	// Local truths, read directly and therefore always current.
	Paused     bool
	WaitingApp bool
	Pinned     bool

	// Survey, refreshed by each pass of the main loop.
	LimitChecked Fact
	Online       Fact
	LimitOK      Fact
	// GoalsMoving: at least one pinned goal has actually spent tokens recently.
	// Status "active" alone is not proof — it says Codex considers the goal
	// started, not that a model is working — and a green lamp over an idle
	// account is exactly the lie this design exists to prevent.
	GoalsMoving Fact
	// Progress says what that movement was, so the window can show it.
	Progress string
}

// Lamp derives the traffic light and its explanation.
func (e Evidence) Lamp(now time.Time) (Lamp, string) {
	switch {
	case e.ServerAlive.At.IsZero():
		return LampRed, "ещё ничего не проверено"
	case e.ServerAlive.Stale(now, LivenessMaxAge):
		return LampRed, "проверка связи остановилась " +
			now.Sub(e.ServerAlive.At).Round(time.Second).String() + " назад"
	case !e.ServerAlive.Proved(now, LivenessMaxAge):
		return LampRed, "нет связи с app-server"

	// Deliberate stops are red: on pause nothing is running, and no amount of
	// fresh evidence about the server changes that.
	case e.Paused:
		return LampRed, "работа остановлена вами (наблюдение продолжается)"
	case e.WaitingApp:
		return LampRed, "ждём, пока вы закроете приложение ChatGPT"

	case !e.Pinned:
		return LampYellow, "ни одна цель не отмечена"

	case e.LimitChecked.At.IsZero():
		return LampYellow, "лимит аккаунта ещё не проверялся"
	case e.LimitChecked.Stale(now, SurveyMaxAge):
		return LampYellow, "лимит не проверялся " +
			now.Sub(e.LimitChecked.At).Round(time.Second).String()
	case !e.Online.Proved(now, SurveyMaxAge):
		return LampYellow, "нет связи с сервером OpenAI"
	case !e.LimitOK.Proved(now, SurveyMaxAge):
		return LampYellow, "лимит аккаунта исчерпан"

	case e.GoalsMoving.Stale(now, SurveyMaxAge):
		return LampYellow, "давно не проверяли, движутся ли цели"
	case !e.GoalsMoving.Proved(now, SurveyMaxAge):
		return LampYellow, "цели числятся запущенными, но токены не тратятся"
	}
	if e.Progress != "" {
		return LampGreen, e.Progress
	}
	return LampGreen, "работа идёт"
}

// GoalLamp is the traffic light for one goal, judged only by what is known
// about that goal.
//
// A single lamp for the whole program said РАБОТАЕТ while one of three goals
// stood blocked: true of the account, misleading about the work. Each goal now
// answers for itself.
func GoalLamp(status, waiting string, spending bool) (Lamp, string) {
	// Live evidence outranks the stored status. A goal answered a block and
	// went back to work — running gh pr merge, spending tokens, reporting
	// «Merge подтверждён» — while thread/goal/get still said blocked. The
	// status lags the thread, and treating it as the truth called working
	// goals stopped.
	if spending {
		return LampGreen, "работает"
	}
	switch {
	case status == "":
		return LampRed, "состояние неизвестно"
	case strings.EqualFold(status, "blocked"):
		return LampRed, "ждёт вашего слова"
	case strings.EqualFold(status, "complete"):
		return LampGreen, "завершена"
	case spending:
		return LampGreen, "работает"
	case waiting != "":
		return LampYellow, waiting
	}
	return LampYellow, "стоит"
}

// Overall combines the goals' own lamps into one.
//
// The headline lamp used to be computed apart from the goals, and said
// РАБОТАЕТ while every goal on screen stood still. A summary that contradicts
// what it summarises is worse than no summary: green needs at least one goal
// actually working, and red means not one of them is.
func Overall(goals []Lamp) (Lamp, string) {
	if len(goals) == 0 {
		return LampRed, "целей нет"
	}
	green, red := 0, 0
	for _, l := range goals {
		switch l {
		case LampGreen:
			green++
		case LampRed:
			red++
		}
	}
	switch {
	case green > 0:
		return LampGreen, fmt.Sprintf("работают %d из %d", green, len(goals))
	case red == len(goals):
		return LampRed, fmt.Sprintf("не работает ни одна из %d", len(goals))
	}
	return LampYellow, fmt.Sprintf("ни одна из %d не работает прямо сейчас", len(goals))
}
