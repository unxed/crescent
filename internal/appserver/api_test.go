package appserver

import (
	"testing"
	"time"
)

// The window shapes are taken from the published schema; both spellings are
// accepted because the response has carried each at different times.
func TestResetTimeFromEitherSpelling(t *testing.T) {
	now := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)

	cases := map[string]RateLimitWindow{
		"snake, absolute": {ResetsAt: "2026-09-08T22:33:00Z"},
		"camel, absolute": {ResetsAtCamel: "2026-09-08T22:33:00Z"},
	}
	want := time.Date(2026, 9, 8, 22, 33, 0, 0, time.UTC)
	for name, w := range cases {
		got, ok := w.ResetAt(now)
		if !ok || !got.Equal(want) {
			t.Errorf("%s: got %v/%v, want %v", name, got, ok, want)
		}
	}

	rel := RateLimitWindow{ResetsInSecs: 3600}
	if got, ok := rel.ResetAt(now); !ok || !got.Equal(now.Add(time.Hour)) {
		t.Errorf("relative: got %v/%v", got, ok)
	}
	if _, ok := (RateLimitWindow{}).ResetAt(now); ok {
		t.Error("время придумано там, где сервер ничего не сказал")
	}
}

// The one question crescent asks before every turn.
func TestExhaustedPicksTheNearestSpentWindow(t *testing.T) {
	now := time.Now()
	limits := RateLimits{
		Primary:   &RateLimitWindow{UsedPercent: 100, ResetsInSecs: 3300},
		Secondary: &RateLimitWindow{UsedPercent: 41, ResetsInSecs: 548000},
	}
	limited, at := limits.Exhausted(now)
	if !limited {
		t.Fatal("исчерпанное окно не замечено")
	}
	if d := time.Until(at); d < 50*time.Minute || d > time.Hour {
		t.Errorf("сброс через %v, ожидалось около 55 минут", d.Round(time.Minute))
	}

	free := RateLimits{Primary: &RateLimitWindow{UsedPercent: 12, ResetsInSecs: 3300}}
	if limited, _ := free.Exhausted(now); limited {
		t.Error("свободное окно принято за исчерпанное")
	}
}

func TestThreadAndGoalAcceptEitherFieldName(t *testing.T) {
	if got := (Thread{ThreadID: "t1", Title: "Konsole"}).Ident(); got != "t1" {
		t.Errorf("Ident = %q", got)
	}
	if got := (Thread{ID: "t1", Title: "Konsole"}).Label(); got != "Konsole" {
		t.Errorf("Label = %q", got)
	}
	if (Goal{}).Set() {
		t.Error("пустая цель считается поставленной")
	}
	if got := (Goal{Text: "починить CI"}).Description(); got != "починить CI" {
		t.Errorf("Description = %q", got)
	}
}
