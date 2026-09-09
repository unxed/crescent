package appserver

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Limits were asked for from the first day and went unshown for weeks, so the
// shape that carries them is worth pinning: the historical single bucket plus
// rateLimitsByLimitId, where a model's own allowance lives.
func TestEveryLimitBucketIsRead(t *testing.T) {
	const body = `{
	  "rateLimits": {
	    "primary":   {"usedPercent": 42, "windowDurationMins": 300,   "resetsAt": 1788906780},
	    "secondary": {"usedPercent": 18, "windowDurationMins": 10080, "resetsAt": 1789488360}
	  },
	  "rateLimitsByLimitId": {
	    "codex-fable": {"limitName": "Fable",
	      "primary": {"usedPercent": 73, "windowDurationMins": 10080, "resetsAt": 1789488360}}
	  },
	  "ordinaryUsageAllowed": true
	}`

	var resp struct {
		RateLimits           RateLimits            `json:"rateLimits"`
		ByLimitID            map[string]RateLimits `json:"rateLimitsByLimitId"`
		OrdinaryUsageAllowed *bool                 `json:"ordinaryUsageAllowed"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	limits := resp.RateLimits
	for id, b := range resp.ByLimitID {
		if b.LimitID == "" {
			b.LimitID = id
		}
		limits.Buckets = append(limits.Buckets, b)
	}

	lines := strings.Join(limits.Summary(), " | ")
	for _, want := range []string{"5-часовое", "42%", "недельное", "18%", "Fable", "73%"} {
		if !strings.Contains(lines, want) {
			t.Errorf("в сводке нет %q:\n%s", want, lines)
		}
	}
	// A model bucket must be named, not shown as another anonymous account line.
	if strings.Count(lines, "аккаунт") != 2 {
		t.Errorf("корзина модели не названа своим именем:\n%s", lines)
	}
}

// A bucket the server did not name still has to be identifiable.
func TestBucketFallsBackToItsIdentifier(t *testing.T) {
	if got := (RateLimits{LimitID: "codex"}).Title(); got != "codex" {
		t.Errorf("Title = %q", got)
	}
	if got := (RateLimits{NormalModelSlug: "gpt-5.6-luna"}).Title(); got != "gpt-5.6-luna" {
		t.Errorf("Title = %q", got)
	}
}

// A deleted or recreated chat leaves its id pinned, and every request for it
// fails the same way for ever. Recognising that is what lets crescent stop
// asking instead of writing one identical line every few seconds — a live log
// carried hundreds of them.
func TestVanishedThreadIsRecognised(t *testing.T) {
	gone := []error{
		errors.New("thread/goal/get: thread not found: 01a04f59-3ac3-7790-9f25-a0bd6c10ca2b (code -32600)"),
		errors.New("Thread Not Found"),
		errors.New("no such thread"),
	}
	for _, err := range gone {
		if !IsThreadGone(err) {
			t.Errorf("не опознано как исчезнувший тред: %v", err)
		}
	}

	// Everything else must keep its pin: a network blip is not a deletion.
	stays := []error{
		nil,
		errors.New("failed to fetch codex rate limits"),
		errors.New("thread 01a0 already has an active writer (code -32600)"),
	}
	for _, err := range stays {
		if IsThreadGone(err) {
			t.Errorf("цель была бы снята из-за временной ошибки: %v", err)
		}
	}
}

// The response repeats the same allowance under several names: the historical
// "account" view and a "codex" bucket carry identical percentages and reset
// times. Printing both stretched the window far past everything else in it.
func TestIdenticalBucketsAreNotRepeated(t *testing.T) {
	same := &RateLimitWindow{UsedPercent: 10, WindowDurationMins: 300,
		ResetsAt: Timestamp{Time: mustTime("2026-09-09T17:36:00Z"), Valid: true}}

	limits := RateLimits{Primary: same}
	limits.Buckets = []RateLimits{
		{LimitName: "codex", Primary: same}, // те же цифры
		{LimitName: "gpt-reserve", Primary: &RateLimitWindow{UsedPercent: 0,
			WindowDurationMins: 10080,
			ResetsAt:           Timestamp{Time: mustTime("2026-09-09T12:56:00Z"), Valid: true}}},
	}

	got := limits.Summary()
	if len(got) != 2 {
		t.Fatalf("строк %d, ожидалось 2 (дубликат codex должен исчезнуть): %q", len(got), got)
	}
	joined := strings.Join(got, " | ")
	if strings.Contains(joined, "codex") {
		t.Errorf("повтор той же корзины остался: %s", joined)
	}
	if !strings.Contains(joined, "gpt-reserve") {
		t.Errorf("корзина с другими цифрами потеряна: %s", joined)
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
