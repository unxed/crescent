package supervisor

import (
	"testing"
	"time"
)

// The design rule: green is earned, never assumed. A Status nobody has filled
// in must be red, the way a train stands still until air pressure proves the
// brakes are released.
func TestNothingProvedIsRed(t *testing.T) {
	var s Status
	if lamp := s.Lamp(); lamp != LampRed {
		t.Fatalf("пустое состояние даёт %v, должно быть красным", lamp)
	}
	if s.Why() == "" {
		t.Error("красная лампа не объясняет себя")
	}
}

// Evidence that stopped being refreshed means the collector died, which is
// indistinguishable from the work having died — so it must read as red.
func TestStaleEvidenceGoesRedOnItsOwn(t *testing.T) {
	now := time.Now()
	full := Evidence{
		At:          now.Add(-EvidenceMaxAge - time.Second),
		ServerAlive: true, LimitKnown: true, LimitOK: true, LimitChecked: true,
		Online: true, GoalsMoving: true, Pinned: true,
	}
	if lamp, why := full.Lamp(now); lamp != LampRed {
		t.Errorf("устаревшее доказательство даёт %v (%s), должно быть красным", lamp, why)
	}
	// The same evidence, fresh, is green.
	full.At = now
	if lamp, why := full.Lamp(now); lamp != LampGreen {
		t.Errorf("свежее полное доказательство даёт %v (%s), должно быть зелёным", lamp, why)
	}
}

// Each missing proof has its own colour and its own explanation.
func TestEachMissingProofDowngradesTheLamp(t *testing.T) {
	now := time.Now()
	base := Evidence{At: now, ServerAlive: true, LimitKnown: true, LimitOK: true,
		LimitChecked: true, Online: true, GoalsMoving: true, Pinned: true}

	cases := []struct {
		name string
		mut  func(*Evidence)
		want Lamp
	}{
		{"сервер молчит", func(e *Evidence) { e.ServerAlive = false }, LampRed},
		{"нет сети", func(e *Evidence) { e.Online = false }, LampYellow},
		{"нет отмеченных целей", func(e *Evidence) { e.Pinned = false }, LampYellow},
		{"лимит не проверялся", func(e *Evidence) { e.LimitChecked = false }, LampYellow},
		{"лимит неизвестен", func(e *Evidence) { e.LimitKnown = false }, LampYellow},
		{"лимит исчерпан", func(e *Evidence) { e.LimitOK = false }, LampYellow},
		{"цели не движутся", func(e *Evidence) { e.GoalsMoving = false }, LampYellow},
	}
	for _, c := range cases {
		e := base
		c.mut(&e)
		lamp, why := e.Lamp(now)
		if lamp != c.want {
			t.Errorf("%s: лампа %v, ожидалась %v", c.name, lamp, c.want)
		}
		if why == "" {
			t.Errorf("%s: причина не названа", c.name)
		}
	}
}
