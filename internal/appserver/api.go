package appserver

import (
	"context"
	"encoding/json"
	"time"
)

// Everything below maps one-to-one onto methods declared in the published
// schema (app-server-protocol/schema/json). Fields are decoded leniently —
// only what crescent needs is named, the rest is ignored — so a server that
// grows a field does not break a client that does not want it.

// RateLimitWindow is one usage window: the rolling one and the weekly one are
// reported side by side.
type RateLimitWindow struct {
	UsedPercent    float64 `json:"used_percent"`
	WindowMinutes  int64   `json:"window_minutes"`
	ResetsAt       string  `json:"resets_at"`
	ResetsInSecs   int64   `json:"resets_in_seconds"`
	LimitName      string  `json:"limit_name"`
	ShortLabel     string  `json:"short_label"`
	ResetsAtCamel  string  `json:"resetsAt"`
	ResetsInCamel  int64   `json:"resetsInSeconds"`
	UsedPercentAlt float64 `json:"usedPercent"`
}

// ResetAt returns when this window opens again, if the server said.
func (w RateLimitWindow) ResetAt(now time.Time) (time.Time, bool) {
	for _, s := range []string{w.ResetsAt, w.ResetsAtCamel} {
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, true
		}
	}
	for _, secs := range []int64{w.ResetsInSecs, w.ResetsInCamel} {
		if secs > 0 {
			return now.Add(time.Duration(secs) * time.Second), true
		}
	}
	return time.Time{}, false
}

// Percent returns how much of the window is spent.
func (w RateLimitWindow) Percent() float64 {
	if w.UsedPercent > 0 {
		return w.UsedPercent
	}
	return w.UsedPercentAlt
}

// RateLimits is the answer to "can we work right now?".
type RateLimits struct {
	Primary   *RateLimitWindow `json:"primary"`
	Secondary *RateLimitWindow `json:"secondary"`
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

// Exhausted reports whether any window is spent, and when the nearest one
// reopens. That is the whole question crescent asks before every turn.
func (r RateLimits) Exhausted(now time.Time) (bool, time.Time) {
	var soonest time.Time
	limited := false
	for _, w := range r.Windows() {
		if w.Percent() < 99.5 {
			continue
		}
		limited = true
		if at, ok := w.ResetAt(now); ok && (soonest.IsZero() || at.Before(soonest)) {
			soonest = at
		}
	}
	return limited, soonest
}

// RateLimits asks the server about the account's usage windows.
func (c *Client) RateLimits(ctx context.Context) (RateLimits, error) {
	raw, err := c.Call(ctx, "account/rateLimits/read", map[string]any{})
	if err != nil {
		return RateLimits{}, err
	}
	// The response has grown wrappers before; look for the windows both at the
	// top level and under a `rateLimits` key.
	var direct RateLimits
	if json.Unmarshal(raw, &direct) == nil && (direct.Primary != nil || direct.Secondary != nil) {
		return direct, nil
	}
	var wrapped struct {
		RateLimits RateLimits `json:"rateLimits"`
		Snake      RateLimits `json:"rate_limits"`
	}
	if json.Unmarshal(raw, &wrapped) == nil {
		if wrapped.RateLimits.Primary != nil || wrapped.RateLimits.Secondary != nil {
			return wrapped.RateLimits, nil
		}
		return wrapped.Snake, nil
	}
	return RateLimits{}, nil
}

// Thread is one conversation as the server describes it.
type Thread struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
	Name     string `json:"name"`
	Title    string `json:"title"`
	Cwd      string `json:"cwd"`
	Archived bool   `json:"archived"`
	Updated  string `json:"updatedAt"`
}

// Ident returns whichever identifier the server used.
func (t Thread) Ident() string {
	if t.ID != "" {
		return t.ID
	}
	return t.ThreadID
}

// Label is the name to show a human.
func (t Thread) Label() string {
	if t.Name != "" {
		return t.Name
	}
	return t.Title
}

// Threads lists conversations. Pagination is followed to the end: a pool that
// silently stopped at the first page would be a pool of whatever fitted.
func (c *Client) Threads(ctx context.Context) ([]Thread, error) {
	var out []Thread
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.Call(ctx, "thread/list", params)
		if err != nil {
			return out, err
		}
		var page struct {
			Threads    []Thread `json:"threads"`
			Items      []Thread `json:"items"`
			NextCursor string   `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &page) != nil {
			return out, nil
		}
		batch := page.Threads
		if len(batch) == 0 {
			batch = page.Items
		}
		out = append(out, batch...)
		if page.NextCursor == "" || len(batch) == 0 {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// Goal is a thread's objective, if it has one.
type Goal struct {
	Objective string `json:"objective"`
	Status    string `json:"status"`
	Text      string `json:"text"`
}

// Set reports whether the thread actually carries a goal.
func (g Goal) Set() bool { return g.Objective != "" || g.Text != "" }

// Description is the goal text, whichever field carried it.
func (g Goal) Description() string {
	if g.Objective != "" {
		return g.Objective
	}
	return g.Text
}

// Goal asks what a thread is working towards.
func (c *Client) Goal(ctx context.Context, threadID string) (Goal, error) {
	raw, err := c.Call(ctx, "thread/goal/get", map[string]any{"threadId": threadID})
	if err != nil {
		return Goal{}, err
	}
	var direct Goal
	if json.Unmarshal(raw, &direct) == nil && direct.Set() {
		return direct, nil
	}
	var wrapped struct {
		Goal Goal `json:"goal"`
	}
	_ = json.Unmarshal(raw, &wrapped)
	return wrapped.Goal, nil
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
