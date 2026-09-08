package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unxed/crescent/internal/codex"
)

// fakeCodex writes the given stdout lines and exits with the given code.
func fakeCodex(t *testing.T, stdout string, exit int) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "codex")
	script := "#!/bin/sh\ncat <<'STREAM'\n" + stdout + "\nSTREAM\nexit " + itoa(exit) + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

func session(t *testing.T) codex.Session {
	return codex.Session{ID: "01a0552e-8698-7961-b9ab-199d982579a2", Cwd: t.TempDir(), Objective: "x", Status: "active"}
}

func TestRunOnceReadsTheEventStream(t *testing.T) {
	// Shapes taken from codex-rs/exec/src/exec_events.rs.
	stream := `{"type":"thread.started","thread_id":"abc"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"Готово, тесты зелёные."}}
{"type":"turn.completed","usage":{"input_tokens":1200,"output_tokens":340,"cached_input_tokens":0,"reasoning_output_tokens":90}}`

	p := DefaultPolicy()
	p.CodexPath = fakeCodex(t, stream, 0)

	var trace bytes.Buffer
	res, err := RunOnce(context.Background(), p, session(t), Options{Trace: &trace, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Lines != 4 || res.Parsed != 4 {
		t.Errorf("lines=%d parsed=%d, want 4/4", res.Lines, res.Parsed)
	}
	if res.InputTokens != 1200 || res.OutputTokens != 340 {
		t.Errorf("tokens = %d/%d, want 1200/340", res.InputTokens, res.OutputTokens)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d", res.ExitCode)
	}
	if res.LastMessage != "Готово, тесты зелёные." {
		t.Errorf("LastMessage = %q", res.LastMessage)
	}
	if res.EventTypes["item.completed:agent_message"] != 1 {
		t.Errorf("item type not folded into the event name: %v", res.EventTypes)
	}
	if res.EventTypes["turn.completed"] != 1 {
		t.Errorf("event types not counted: %v", res.EventTypes)
	}
	if res.UsageLimited {
		t.Error("a clean run was reported as usage-limited")
	}
	if !strings.Contains(trace.String(), "thread.started") {
		t.Errorf("trace is empty or wrong:\n%s", trace.String())
	}
}

// The one outcome crescent exists for. The wrapper around this message has
// changed before, so it is detected from the whole line, not one field.
func TestRunOnceDetectsUsageLimitAndReset(t *testing.T) {
	// The real event carries only a message: no error code, no timestamp, so
	// the reset moment has to come out of the sentence.
	at := time.Now().Add(4 * time.Hour).Truncate(time.Minute)
	stream := `{"type":"turn.failed","error":{"message":"You have hit your usage limit. Try again at ` +
		at.Format("3:04 PM") + `."}}`

	p := DefaultPolicy()
	p.CodexPath = fakeCodex(t, stream, 1)

	res, err := RunOnce(context.Background(), p, session(t), Options{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !res.UsageLimited {
		t.Fatal("usage limit not detected")
	}
	if res.ResetsAt.IsZero() {
		t.Fatal("reset time not recovered from the message")
	}
	if d := res.ResetsAt.Sub(at); d < -time.Minute || d > time.Minute {
		t.Errorf("ResetsAt = %v, want about %v", res.ResetsAt, at)
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want the non-zero exit preserved", res.ExitCode)
	}
	if len(res.Errors) == 0 {
		t.Error("error text not collected")
	}
}

// A read-only first run must be provably unable to write anything.
func TestReadOnlyOptionDropsWriteAccess(t *testing.T) {
	p := DefaultPolicy()
	p.CodexPath = fakeCodex(t, `{"type":"turn.completed"}`, 0)
	p.WritableRoots = []string{"/home/u/go/pkg/mod"}

	res, err := RunOnce(context.Background(), p, session(t), Options{ReadOnly: true, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Command, "--sandbox read-only") {
		t.Errorf("sandbox not forced read-only: %s", res.Command)
	}
	if strings.Contains(res.Command, "writable_roots") {
		t.Errorf("writable roots survived a read-only run: %s", res.Command)
	}
}

// Event payloads can carry a whole file; the scanner's default limit would
// truncate them into invalid JSON and silently lose the rest of the turn.
func TestRunOnceHandlesVeryLongLines(t *testing.T) {
	big := strings.Repeat("x", 300_000)
	stream := `{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"` + big + `"}}`

	p := DefaultPolicy()
	p.CodexPath = fakeCodex(t, stream, 0)

	res, err := RunOnce(context.Background(), p, session(t), Options{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Parsed != 1 {
		t.Fatalf("parsed = %d, want 1: a 300 KB event was dropped", res.Parsed)
	}
}

// The raw capture is how the parser gets corrected when the schema moves.
func TestRawCaptureIsVerbatim(t *testing.T) {
	stream := `{"type":"a"}
{"type":"b"}`
	p := DefaultPolicy()
	p.CodexPath = fakeCodex(t, stream, 0)

	var raw bytes.Buffer
	if _, err := RunOnce(context.Background(), p, session(t), Options{RawTo: &raw, Timeout: 20 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(raw.String()); got != stream {
		t.Errorf("raw capture altered the stream:\n%s", got)
	}
}
