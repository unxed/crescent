package appserver

import (
	"encoding/json"
	"testing"
	"time"
)

// Field names here are copied from the published response schemas, not guessed.
// The first attempt guessed the wrappers, got clean answers from a real server
// and reported nothing at all — a failure that looks exactly like success.
func TestRateLimitWindowFromSchemaShape(t *testing.T) {
	w := RateLimitWindow{
		ResetsAt:           Timestamp{Time: time.Date(2026, 9, 8, 22, 33, 0, 0, time.UTC), Valid: true},
		UsedPercent:        100,
		WindowDurationMins: 300,
	}
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
	soon := Timestamp{Time: time.Now().Add(55 * time.Minute), Valid: true}
	late := Timestamp{Time: time.Now().Add(150 * time.Hour), Valid: true}

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
	soon := Timestamp{Time: time.Now().Add(30 * time.Minute), Valid: true}
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

// The schema types resetsAt as a string; the running server sends a number.
// Both have to work, or the client is right about the documentation and wrong
// about reality.
func TestTimestampAcceptsStringAndNumber(t *testing.T) {
	want := time.Date(2026, 9, 8, 22, 33, 0, 0, time.UTC)
	cases := map[string]string{
		"строка RFC3339": `"2026-09-08T22:33:00Z"`,
		"секунды":        `1788906780`,
		"миллисекунды":   `1788906780000`,
	}
	for name, body := range cases {
		var ts Timestamp
		if err := ts.UnmarshalJSON([]byte(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !ts.Valid || !ts.Time.UTC().Truncate(time.Second).Equal(want) {
			t.Errorf("%s: got %v/%v, want %v", name, ts.Time.UTC(), ts.Valid, want)
		}
	}
	for _, body := range []string{"null", `""`, "0"} {
		var ts Timestamp
		if err := ts.UnmarshalJSON([]byte(body)); err != nil || ts.Valid {
			t.Errorf("%s принят за время", body)
		}
	}
}

// The response below is trimmed from a live server. status is an object, not a
// string, and declaring it as a string made the whole page fail to decode — a
// failure that, being swallowed, was reported as "no threads".
func TestThreadListDecodesTheRealShape(t *testing.T) {
	const body = `{"data":[{"id":"01a08208-2013-7922-95c2-34c9cd13f424",` +
		`"sessionId":"01a08208-2013-7922-95c2-34c9cd13f424","preview":"Ты — Лунобот-2",` +
		`"model":"gpt-5.6-luna","createdAt":1788887965,"updatedAt":1788896500,` +
		`"status":{"type":"notLoaded"},"path":"/home/u/.codex/sessions/x.jsonl",` +
		`"cwd":"/home/u/f4","name":"Следовать инструкции Лунобот-2","turns":[]}],"nextCursor":null}`

	var page struct {
		Data       []Thread `json:"data"`
		NextCursor string   `json:"nextCursor"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("настоящий ответ не разобрался: %v", err)
	}
	if len(page.Data) != 1 {
		t.Fatalf("тредов %d, ожидался 1", len(page.Data))
	}
	th := page.Data[0]
	if th.Label() != "Следовать инструкции Лунобот-2" {
		t.Errorf("Label = %q", th.Label())
	}
	if th.Status.Type != "notLoaded" || th.Loaded() {
		t.Errorf("статус = %+v, тред не должен считаться загруженным", th.Status)
	}
	if th.Cwd != "/home/u/f4" {
		t.Errorf("Cwd = %q", th.Cwd)
	}
}

// The statuses below were reported by a live server. usageLimited is the state
// crescent exists to undo, so it must be restartable; a finished or blocked
// goal must not be.
func TestRestartableStatuses(t *testing.T) {
	for _, s := range []string{StatusUsageLimited, StatusPaused, StatusActive, "somethingNew"} {
		if !Restartable(s) {
			t.Errorf("%q должен перезапускаться", s)
		}
	}
	for _, s := range []string{StatusComplete, StatusBlocked, "Completed", "cancelled"} {
		if Restartable(s) {
			t.Errorf("%q перезапускать нельзя", s)
		}
	}
}
