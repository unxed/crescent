package appserver

import (
	"testing"
	"time"
)

// Field names here are copied from the published response schemas, not guessed.
// The first attempt guessed the wrappers, got clean answers from a real server
// and reported nothing at all — a failure that looks exactly like success.
func TestRateLimitWindowFromSchemaShape(t *testing.T) {
	w := RateLimitWindow{ResetsAt: "2026-09-08T22:33:00Z", UsedPercent: 100, WindowDurationMins: 300}
	got, ok := w.ResetAt()
	if !ok || !got.Equal(time.Date(2026, 9, 8, 22, 33, 0, 0, time.UTC)) {
		t.Errorf("ResetAt = %v/%v", got, ok)
	}
	if w.Label() != "5-часовое" {
		t.Errorf("Label = %q", w.Label())
	}
	if (RateLimitWindow{WindowDurationMins: 10080}).Label() != "недельное" {
		t.Error("недельное окно не опознано")
	}
	if _, ok := (RateLimitWindow{}).ResetAt(); ok {
		t.Error("время придумано там, где сервер ничего не сказал")
	}
}

func TestExhaustedPicksTheNearestSpentWindow(t *testing.T) {
	soon := time.Now().Add(55 * time.Minute).UTC().Format(time.RFC3339)
	late := time.Now().Add(150 * time.Hour).UTC().Format(time.RFC3339)

	limits := RateLimits{
		Primary:   &RateLimitWindow{UsedPercent: 100, ResetsAt: soon, WindowDurationMins: 300},
		Secondary: &RateLimitWindow{UsedPercent: 41, ResetsAt: late, WindowDurationMins: 10080},
	}
	limited, at := limits.Exhausted()
	if !limited {
		t.Fatal("исчерпанное окно не замечено")
	}
	if d := time.Until(at); d < 50*time.Minute || d > time.Hour {
		t.Errorf("сброс через %v, ожидалось около 55 минут", d.Round(time.Minute))
	}

	free := RateLimits{Primary: &RateLimitWindow{UsedPercent: 12, ResetsAt: soon}}
	if limited, _ := free.Exhausted(); limited {
		t.Error("свободное окно принято за исчерпанное")
	}
}

// The schema says plainly that clients must not infer recovery from
// percentages or reset times. When the backend states its verdict, it wins.
func TestBackendVerdictOverridesArithmetic(t *testing.T) {
	no := false
	soon := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	limits := RateLimits{
		Primary:              &RateLimitWindow{UsedPercent: 3, ResetsAt: soon},
		OrdinaryUsageAllowed: &no,
	}
	limited, at := limits.Exhausted()
	if !limited {
		t.Fatal("запрет сервера проигнорирован в пользу процентов")
	}
	if at.IsZero() {
		t.Error("время сброса потеряно")
	}
}

func TestGoalAndThreadLabels(t *testing.T) {
	if (Goal{}).Set() {
		t.Error("пустая цель считается поставленной")
	}
	if !(Goal{Objective: "починить CI"}).Set() {
		t.Error("поставленная цель не опознана")
	}
	if got := (Thread{Preview: "первая реплика"}).Label(); got != "первая реплика" {
		t.Errorf("Label = %q, ожидался откат к preview", got)
	}
	if got := (Thread{Name: "Konsole", Preview: "x"}).Label(); got != "Konsole" {
		t.Errorf("Label = %q", got)
	}
}
