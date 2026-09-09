package supervisor

import (
	"testing"
	"time"
)

// full is evidence in which everything has just been established.
func full(now time.Time) Evidence {
	return Evidence{
		ServerAlive:  Fact{OK: true, At: now},
		Pinned:       true,
		LimitChecked: Fact{OK: true, At: now},
		Online:       Fact{OK: true, At: now},
		LimitOK:      Fact{OK: true, At: now},
		GoalsMoving:  Fact{OK: true, At: now},
	}
}

// The checklist from the table in evidence.go, one row per case. Green is
// reachable only through the last row; every other condition must keep it off.
func TestEveryStateInTheTable(t *testing.T) {
	now := time.Now()
	old := now.Add(-SurveyMaxAge - time.Second)
	stale := now.Add(-LivenessMaxAge - time.Second)

	cases := []struct {
		name string
		mut  func(*Evidence)
		want Lamp
	}{
		{"ничего не установлено", func(e *Evidence) { *e = Evidence{} }, LampRed},
		{"связь не проверялась давно", func(e *Evidence) { e.ServerAlive.At = stale }, LampRed},
		{"сервер не отвечает", func(e *Evidence) { e.ServerAlive = Fact{OK: false, At: now} }, LampRed},
		{"пауза", func(e *Evidence) { e.Paused = true }, LampRed},
		{"ждём закрытия приложения", func(e *Evidence) { e.WaitingApp = true }, LampRed},
		{"нет отмеченных целей", func(e *Evidence) { e.Pinned = false }, LampYellow},
		{"лимит не спрашивали", func(e *Evidence) { e.LimitChecked = Fact{} }, LampYellow},
		{"сведения о лимите устарели", func(e *Evidence) { e.LimitChecked.At = old }, LampYellow},
		{"нет связи с OpenAI", func(e *Evidence) { e.Online = Fact{OK: false, At: now} }, LampYellow},
		{"лимит исчерпан", func(e *Evidence) { e.LimitOK = Fact{OK: false, At: now} }, LampYellow},
		{"движение целей не проверяли", func(e *Evidence) { e.GoalsMoving.At = old }, LampYellow},
		{"цели не движутся", func(e *Evidence) { e.GoalsMoving = Fact{OK: false, At: now} }, LampYellow},
		{"всё доказано", func(e *Evidence) {}, LampGreen},
	}

	for _, c := range cases {
		e := full(now)
		c.mut(&e)
		lamp, why := e.Lamp(now)
		if lamp != c.want {
			t.Errorf("%s: лампа %v (%s), ожидалась %v", c.name, lamp, why, c.want)
		}
		if why == "" {
			t.Errorf("%s: причина не названа", c.name)
		}
	}
}

// The defect this design exists to prevent: the heartbeat proves only that the
// server answers, and must not extend the life of any other fact. Sharing one
// timestamp is what left the lamp green through a pause and through a survey
// that had stopped running.
func TestHeartbeatDoesNotRevalidateOtherFacts(t *testing.T) {
	now := time.Now()
	e := full(now)

	// The survey stopped long ago; only the heartbeat is still running.
	e.LimitChecked.At = now.Add(-SurveyMaxAge - time.Second)
	e.LimitOK.At = now.Add(-SurveyMaxAge - time.Second)
	e.GoalsMoving.At = now.Add(-SurveyMaxAge - time.Second)
	e.ServerAlive.At = now

	if lamp, why := e.Lamp(now); lamp == LampGreen {
		t.Fatalf("пульс продлил жизнь чужим фактам: %v (%s)", lamp, why)
	}
}

// Pause is a deliberate stop, so no amount of fresh evidence makes it green.
func TestPauseIsRedNoMatterHowFreshEverythingElseIs(t *testing.T) {
	now := time.Now()
	e := full(now)
	e.Paused = true
	if lamp, why := e.Lamp(now); lamp != LampRed {
		t.Errorf("на паузе лампа %v (%s), должна быть красной", lamp, why)
	}
}

// A fact that was never established and one that was established false both
// keep the lamp off green, but they are different situations and must read
// differently.
func TestUnknownAndFalseAreBothNotGreenButDiffer(t *testing.T) {
	now := time.Now()

	unknown := full(now)
	unknown.LimitChecked = Fact{}
	_, whyUnknown := unknown.Lamp(now)

	failed := full(now)
	failed.Online = Fact{OK: false, At: now}
	_, whyFailed := failed.Lamp(now)

	if whyUnknown == whyFailed {
		t.Errorf("«не спрашивали» и «спросили и не смогли» звучат одинаково: %q", whyUnknown)
	}
}
