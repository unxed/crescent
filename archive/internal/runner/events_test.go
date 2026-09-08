package runner

import (
	"testing"
	"time"
)

// The wordings below are taken from codex-rs/protocol/src/error.rs, not
// invented: every usage-limit variant opens with "You've hit your usage limit",
// and credit exhaustion is worded differently on purpose.
func TestFailureClassification(t *testing.T) {
	cases := map[string]Failure{
		"You've hit your usage limit. Try again at 3:51 PM.":                                             FailUsageLimit,
		"You've hit your usage limit. Try again later.":                                                  FailUsageLimit,
		"You've hit your usage limit for gpt-5.6. Switch to another model now, or try again at 9:01 PM.": FailUsageLimit,
		"You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again later.": FailUsageLimit,
		"Your workspace is out of credits. Add credits to continue.": FailOutOfCredits,
		"stream disconnected before completion":                      FailOther,
		"":                                                           FailNone,
	}
	for msg, want := range cases {
		if got := classifyFailure(msg); got != want {
			t.Errorf("classifyFailure(%.40q) = %v, want %v", msg, got, want)
		}
	}
}

// Running out of credits is not a limit: no amount of waiting opens that
// window, and a daemon that confused the two would sit forever.
func TestOutOfCreditsIsNotAWait(t *testing.T) {
	if classifyFailure("Your workspace is out of credits. Add credits to continue.") == FailUsageLimit {
		t.Fatal("credit exhaustion was taken for a usage limit")
	}
}

func TestParseRetryAtSameDay(t *testing.T) {
	now := time.Date(2026, 9, 8, 13, 0, 0, 0, time.Local)
	got, ok := parseRetryAt("You've hit your usage limit. Try again at 3:51 PM.", now)
	if !ok {
		t.Fatal("time not parsed")
	}
	want := time.Date(2026, 9, 8, 15, 51, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A time of day already past means tomorrow: a limit never resets backwards.
func TestParseRetryAtRollsOverToTomorrow(t *testing.T) {
	now := time.Date(2026, 9, 8, 23, 30, 0, 0, time.Local)
	got, ok := parseRetryAt("You've hit your usage limit. Try again at 1:15 AM.", now)
	if !ok {
		t.Fatal("time not parsed")
	}
	if got.Day() != 9 || got.Hour() != 1 {
		t.Errorf("got %v, want 1:15 on the 9th", got)
	}
}

// The long form carries an ordinal suffix no Go layout describes.
func TestParseRetryAtLongForm(t *testing.T) {
	now := time.Date(2026, 2, 1, 12, 0, 0, 0, time.Local)
	got, ok := parseRetryAt("You've hit your usage limit. Try again at Feb 23rd, 2026 9:01 PM.", now)
	if !ok {
		t.Fatal("time not parsed")
	}
	want := time.Date(2026, 2, 23, 21, 1, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseRetryAtAbsentWhenCodexSaysLater(t *testing.T) {
	if _, ok := parseRetryAt("You've hit your usage limit. Try again later.", time.Now()); ok {
		t.Error("a time was invented where the message gives none")
	}
}

// Seen on a live machine, and not in the event stream at all — on stderr:
//
//	thread-store conflict: thread <uuid> already has an active writer
//
// The desktop app keeps a thread locked while it is open. Treating that as a
// generic failure meant the daemon retried it every tick, forever, and reported
// "codex printed nothing" as the diagnosis.
func TestThreadConflictIsItsOwnFailure(t *testing.T) {
	msgs := []string{
		"Error: thread/resume: thread/resume failed: thread 01a08208-2013-7922-95c2-34c9cd13f424 already has an active writer (code -32600)",
		"ERROR codex_core::session: Failed to create session: thread-store conflict: thread 01a0 already has an active writer",
	}
	for _, m := range msgs {
		if got := classifyFailure(m); got != FailBusy {
			t.Errorf("classifyFailure(%.50q) = %v, want FailBusy", m, got)
		}
	}
	// And it must not be mistaken for the one failure that means "wait".
	if classifyFailure(msgs[0]) == FailUsageLimit {
		t.Error("a busy thread was taken for a usage limit")
	}
}
