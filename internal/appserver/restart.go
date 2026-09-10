package appserver

import (
	"context"
	"regexp"
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
		// A goal that cannot be restarted is still a goal, and hiding it was
		// worse than useless: Лунобот-1 sat blocked, waiting for a word from
		// the user, and did not appear in the window at all. Whether to restart
		// is decided later, by status; whether to show is decided here, and the
		// answer is always yes.
		if err != nil || !goal.Set() {
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

// IsThreadGone reports an error meaning the thread no longer exists.
//
// A chat deleted or recreated leaves its id pinned, and asking about it fails
// the same way for ever. Recognising it is what lets crescent stop asking
// instead of filling the journal with one line every few seconds.
func IsThreadGone(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "thread not found") ||
		strings.Contains(low, "no such thread")
}

// threadIDRe finds a thread id inside a server log line, so a diagnostic can be
// filed against the goal it concerns instead of a общий поток.
var threadIDRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// ThreadIDIn returns the thread id mentioned in a line, if any.
//
// Only ids introduced as a thread count. A log line also carries an
// installation_id in the same UUID shape, and taking the first match created a
// goal journal named after the installation — a file for a goal that does not
// exist.
func ThreadIDIn(line string) string {
	for _, key := range []string{"thread_id=", "threadId\":\"", "thread_id\":\"", "rollout for ", "projection for "} {
		if i := strings.Index(line, key); i >= 0 {
			if id := threadIDRe.FindString(line[i+len(key):]); id != "" {
				return id
			}
		}
	}
	return ""
}

// ordinalRe matches the thread-store projection mismatch.
var ordinalRe = regexp.MustCompile(`expected ordinal (\d+), got (\d+)`)

// IsHistoryDesynced reports a thread whose history database has diverged from
// its rollout file.
//
// Seen on a live machine, hundreds of times per thread and always off by one:
//
//	thread history projection for <id> expected ordinal 4330, got 4329
//
// The SQLite projection believes it has already applied a line that the rollout
// file is offering again, so the write is refused, the turn is shut down from
// inside Codex, and nothing runs. Restarting such a goal cannot help: every
// turn dies the same way. Recognising it is what lets crescent stop trying and
// say what is actually wrong.
func IsHistoryDesynced(line string) bool {
	return ordinalRe.MatchString(line) ||
		strings.Contains(line, "is behind durable rollout")
}
