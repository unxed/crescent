package appserver

import (
	"context"
	"strings"
	"time"
)

// GoalStatus values the server actually reports. Seen on a live account:
// usageLimited is exactly the state crescent exists to undo.
const (
	StatusUsageLimited = "usageLimited"
	StatusPaused       = "paused"
	StatusActive       = "active"
	StatusComplete     = "complete"
	StatusBlocked      = "blocked"
)

// Restartable reports whether pushing this goal would do any good. A finished
// goal has nothing left; a blocked one needs a person, not another turn.
func Restartable(status string) bool {
	switch strings.ToLower(status) {
	case strings.ToLower(StatusComplete), strings.ToLower(StatusBlocked), "completed", "cancelled", "canceled":
		return false
	}
	return true
}

// Candidate is a goal worth restarting.
type Candidate struct {
	Thread Thread
	Goal   Goal
}

// Candidates lists the goals that are waiting to be pushed on, freshest first.
//
// The server names the state itself, so nothing here is inferred: a goal that
// stopped on a usage limit says so.
func (c *Client) Candidates(ctx context.Context) ([]Candidate, error) {
	threads, _, err := c.Threads(ctx)
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, t := range threads {
		if t.ID == "" {
			continue
		}
		goal, err := c.Goal(ctx, t.ID)
		if err != nil || !goal.Set() || !Restartable(goal.Status) {
			continue
		}
		out = append(out, Candidate{Thread: t, Goal: goal})
	}
	return out, nil
}

// Restart pushes one goal forward: load the thread, then send it a turn.
//
// Resume comes first because a thread listed as notLoaded has no session for a
// turn to belong to.
func (c *Client) Restart(ctx context.Context, threadID, prompt string) error {
	if err := c.Resume(ctx, threadID); err != nil {
		return err
	}
	return c.StartTurn(ctx, threadID, prompt)
}

// RateLimitsWithRetry asks for the usage windows, retrying a fetch that failed
// upstream.
//
// The read goes out to the backend, and that request can simply fail — seen as
// "error sending request for url …/wham/usage". A transient network error must
// not be mistaken for an answer, and above all must not be mistaken for
// permission: an unknown state means wait, never go.
func (c *Client) RateLimitsWithRetry(ctx context.Context, attempts int) (RateLimits, error) {
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return RateLimits{}, ctx.Err()
			case <-time.After(time.Duration(1<<uint(i-1)) * 2 * time.Second):
			}
		}
		limits, _, err := c.RateLimits(ctx)
		if err == nil {
			return limits, nil
		}
		last = err
	}
	return RateLimits{}, last
}
