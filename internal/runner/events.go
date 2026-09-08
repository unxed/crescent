package runner

import (
	"regexp"
	"strings"
	"time"
)

// The shape of `codex exec --json` is no longer guessed. It is defined in
// codex-rs/exec/src/exec_events.rs in the Codex repository, which is public, so
// the parser below follows the actual types:
//
//	{"type":"thread.started","thread_id":"..."}
//	{"type":"turn.started"}
//	{"type":"turn.completed","usage":{"input_tokens":…,"output_tokens":…,…}}
//	{"type":"turn.failed","error":{"message":"…"}}
//	{"type":"item.started"|"item.updated"|"item.completed","item":{"id":…,"type":…}}
//	{"type":"error","message":"…"}
//
// Two corrections to what was assumed before, both of which matter:
//
//   - a failed turn carries only `error.message`. There is no machine-readable
//     error code in this stream — CodexErrorInfo exists in the protocol but is
//     not what `exec` emits — so a usage limit has to be recognised from the
//     text;
//   - that text carries no reset timestamp in machine form either. It may say
//     "Try again at 3:51 PM" in local time, and Codex separately writes the
//     rate-limit snapshot into the session, which is the more reliable source.
const (
	ItemAgentMessage     = "agent_message"
	ItemReasoning        = "reasoning"
	ItemCommandExecution = "command_execution"
	ItemFileChange       = "file_change"
	ItemWebSearch        = "web_search"
	ItemTodoList         = "todo_list"
)

// Failure classifies why a turn failed, because the right reaction differs.
type Failure uint8

// Kinds of failure crescent must tell apart.
const (
	FailNone Failure = iota
	// FailUsageLimit: waiting is exactly the right response.
	FailUsageLimit
	// FailOutOfCredits: waiting will never help. Codex says "out of credits",
	// and a daemon that treats it as a limit would wait for a window that is
	// never going to open.
	FailOutOfCredits
	// FailOther: some other error; the message is what there is.
	FailOther
)

// classifyFailure reads the message Codex renders for a failed turn.
//
// The wordings come from codex-rs/protocol/src/error.rs: every usage-limit
// variant begins "You've hit your usage limit", optionally naming a limit, and
// the credit-exhaustion variants say the workspace is out of credits.
func classifyFailure(msg string) Failure {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "out of credits"):
		return FailOutOfCredits
	case strings.Contains(low, "hit your usage limit"),
		strings.Contains(low, "usage limit"),
		strings.Contains(low, "rate limit"):
		return FailUsageLimit
	case msg != "":
		return FailOther
	}
	return FailNone
}

// Codex appends the reset time to the message in one of two local-time forms:
//
//	" Try again at 3:51 PM."                  same day
//	" Try again at Feb 23rd, 2026 9:01 PM."   another day
//
// (also "or try again at …" in the switch-model variant).
var retryAtRe = regexp.MustCompile(`(?i)try again at ([^.]+)\.`)

// parseRetryAt extracts the reset moment from the message, in local time.
//
// This is a convenience, not the source of truth: Codex writes the rate-limit
// snapshot into the session when a limit is hit, and reading it back is exact.
// Parsing the sentence just gets the answer a little sooner.
func parseRetryAt(msg string, now time.Time) (time.Time, bool) {
	m := retryAtRe.FindStringSubmatch(msg)
	if m == nil {
		return time.Time{}, false
	}
	text := strings.TrimSpace(m[1])

	// The long form carries an ordinal suffix that no Go layout describes.
	cleaned := ordinalRe.ReplaceAllString(text, "$1")

	for _, layout := range []string{"Jan 2, 2006 3:04 PM", "Jan 2, 2006 3:04PM"} {
		if t, err := time.ParseInLocation(layout, cleaned, time.Local); err == nil {
			return t, true
		}
	}
	// The short form is a time of day: today if still ahead, tomorrow if past,
	// because a limit never resets into the past.
	for _, layout := range []string{"3:04 PM", "3:04PM"} {
		t, err := time.ParseInLocation(layout, cleaned, time.Local)
		if err != nil {
			continue
		}
		at := time.Date(now.Year(), now.Month(), now.Day(),
			t.Hour(), t.Minute(), 0, 0, time.Local)
		if at.Before(now) {
			at = at.AddDate(0, 0, 1)
		}
		return at, true
	}
	return time.Time{}, false
}

var ordinalRe = regexp.MustCompile(`(\d+)(st|nd|rd|th)`)
