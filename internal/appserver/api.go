package appserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Everything below maps one-to-one onto methods declared in the published
// schema (app-server-protocol/schema/json). Fields are decoded leniently —
// only what crescent needs is named, the rest is ignored — so a server that
// grows a field does not break a client that does not want it.

// Field names come from the published response schemas, which I should have
// read the first time: RateLimitWindow is {resetsAt, usedPercent,
// windowDurationMins}, the snapshot wraps {primary, secondary}, and the
// response wraps that under `rateLimits`. Guessing at the wrapper produced a
// client that got clean answers and found nothing in them.
type RateLimitWindow struct {
	ResetsAt           string  `json:"resetsAt"`
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int64   `json:"windowDurationMins"`
}

// ResetAt returns when this window opens again, if the server said.
func (w RateLimitWindow) ResetAt() (time.Time, bool) {
	if w.ResetsAt == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, w.ResetsAt); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Label names the window by its length: a five-hour rolling allowance and a
// weekly one sit side by side, and the duration is what tells them apart.
func (w RateLimitWindow) Label() string {
	switch {
	case w.WindowDurationMins == 0:
		return "окно"
	case w.WindowDurationMins >= 7*24*60:
		return "недельное"
	case w.WindowDurationMins >= 24*60:
		return "суточное"
	default:
		return fmt.Sprintf("%d-часовое", w.WindowDurationMins/60)
	}
}

// RateLimits is the answer to "can we work right now?".
type RateLimits struct {
	Primary   *RateLimitWindow `json:"primary"`
	Secondary *RateLimitWindow `json:"secondary"`

	// OrdinaryUsageAllowed is the backend's own verdict. The schema is explicit
	// that clients must not infer recovery from percentages or reset times, so
	// when it is present it wins over any arithmetic of ours.
	OrdinaryUsageAllowed *bool `json:"-"`
}

// Windows returns the windows that are present, primary first.
func (r RateLimits) Windows() []RateLimitWindow {
	var out []RateLimitWindow
	if r.Primary != nil {
		out = append(out, *r.Primary)
	}
	if r.Secondary != nil {
		out = append(out, *r.Secondary)
	}
	return out
}

// Exhausted reports whether work is possible now, and when it will be.
func (r RateLimits) Exhausted() (bool, time.Time) {
	if r.OrdinaryUsageAllowed != nil && !*r.OrdinaryUsageAllowed {
		_, at := r.nearestReset()
		return true, at
	}
	var soonest time.Time
	limited := false
	for _, w := range r.Windows() {
		if w.UsedPercent < 99.5 {
			continue
		}
		limited = true
		if at, ok := w.ResetAt(); ok && (soonest.IsZero() || at.Before(soonest)) {
			soonest = at
		}
	}
	return limited, soonest
}

func (r RateLimits) nearestReset() (bool, time.Time) {
	var soonest time.Time
	for _, w := range r.Windows() {
		if at, ok := w.ResetAt(); ok && (soonest.IsZero() || at.Before(soonest)) {
			soonest = at
		}
	}
	return !soonest.IsZero(), soonest
}

// RateLimits asks the server about the account's usage windows.
func (c *Client) RateLimits(ctx context.Context) (RateLimits, json.RawMessage, error) {
	raw, err := c.Call(ctx, "account/rateLimits/read", map[string]any{})
	if err != nil {
		return RateLimits{}, raw, err
	}
	var resp struct {
		RateLimits           RateLimits `json:"rateLimits"`
		OrdinaryUsageAllowed *bool      `json:"ordinaryUsageAllowed"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return RateLimits{}, raw, err
	}
	limits := resp.RateLimits
	limits.OrdinaryUsageAllowed = resp.OrdinaryUsageAllowed
	return limits, raw, nil
}

// Thread is one conversation, named as the schema names it.
type Thread struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Cwd     string `json:"cwd"`
	Status  string `json:"status"`
	Preview string `json:"preview"`
	Updated string `json:"updatedAt"`
}

// Label is the name to show a human, falling back to the preview line.
func (t Thread) Label() string {
	if t.Name != "" {
		return t.Name
	}
	return t.Preview
}

// Threads lists conversations. Pagination is followed to the end: a pool that
// silently stopped at the first page would be a pool of whatever fitted.
func (c *Client) Threads(ctx context.Context) ([]Thread, json.RawMessage, error) {
	var out []Thread
	var first json.RawMessage
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.Call(ctx, "thread/list", params)
		if first == nil {
			first = raw
		}
		if err != nil {
			return out, first, err
		}
		// The array is `data`. Reading `threads` produced a client that asked
		// correctly, was answered correctly, and reported nothing.
		var page struct {
			Data       []Thread `json:"data"`
			NextCursor string   `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &page) != nil {
			return out, first, nil
		}
		out = append(out, page.Data...)
		if page.NextCursor == "" || len(page.Data) == 0 {
			return out, first, nil
		}
		cursor = page.NextCursor
	}
}

// Goal is a thread's objective, named as the schema names it.
type Goal struct {
	Objective       string `json:"objective"`
	Status          string `json:"status"`
	ThreadID        string `json:"threadId"`
	TimeUsedSeconds int64  `json:"timeUsedSeconds"`
	TokensUsed      int64  `json:"tokensUsed"`
}

// Set reports whether the thread actually carries a goal.
func (g Goal) Set() bool { return g.Objective != "" }

// Goal asks what a thread is working towards.
func (c *Client) Goal(ctx context.Context, threadID string) (Goal, error) {
	raw, err := c.Call(ctx, "thread/goal/get", map[string]any{"threadId": threadID})
	if err != nil {
		return Goal{}, err
	}
	var wrapped struct {
		Goal *Goal `json:"goal"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil || wrapped.Goal == nil {
		return Goal{}, nil
	}
	return *wrapped.Goal, nil
}

// Resume loads a thread so that turns can be started on it.
func (c *Client) Resume(ctx context.Context, threadID string) error {
	_, err := c.Call(ctx, "thread/resume", map[string]any{"threadId": threadID})
	return err
}

// StartTurn sends a message to a resumed thread — the nudge that makes a
// paused goal move again.
func (c *Client) StartTurn(ctx context.Context, threadID, text string) error {
	_, err := c.Call(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input": []any{
			map[string]any{"type": "text", "text": text},
		},
	})
	return err
}
