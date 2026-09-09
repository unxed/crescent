package appserver

import (
	"encoding/json"
	"strings"
	"testing"
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
