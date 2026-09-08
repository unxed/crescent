package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loopEnv(t *testing.T, rollouts map[string]string, stream string, exit int) (*Loop, string) {
	t.Helper()
	setCacheDir(t, t.TempDir())

	home := t.TempDir()
	dir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)

	for name, body := range rollouts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The scanner yields to a human when a rollout was touched recently.
	old := time.Now().Add(-48 * time.Hour)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		os.Chtimes(filepath.Join(dir, e.Name()), old, old)
	}

	p := mockCodex(t, stream, exit)
	p.QuietPeriod = time.Minute

	l, err := NewLoop(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	l.Log = io.Discard
	l.Tick = 10 * time.Millisecond
	return l, dir
}

const oneGoal = `{"type":"session_meta","cwd":"CWD"}
{"type":"goal","payload":{"goal":{"objective":"довести Konsole до запуска","status":"active"}}}`

func goalRollout(t *testing.T) map[string]string {
	t.Helper()
	// jsonPath, not plain concatenation: a Windows temp directory is full of
	// backslashes, which are escapes inside a JSON string.
	return map[string]string{
		"rollout-a-01a04f59-3ac3-7790-9f25-a0bd6c10ca2b.jsonl": strings.Replace(oneGoal, "CWD", jsonPath(t.TempDir()), 1),
	}
}

// The self-check exists so that "does resume work here?" is answered by the
// program, not by asking the user to run something.
func TestProbeFailsLoudlyWhenCodexPrintsNothing(t *testing.T) {
	l, _ := loopEnv(t, goalRollout(t), "", 3)

	err := l.Run(context.Background())
	if err == nil {
		t.Fatal("a silent codex was treated as a working one")
	}
	if !strings.Contains(err.Error(), "ни строки") {
		t.Errorf("err = %v", err)
	}
	// The status file must survive the failure and say why.
	s, ok := ReadStatus()
	if !ok {
		t.Fatal("the daemon removed its status file after failing")
	}
	if s.State != StateFailed || !strings.Contains(s.Message, "ни строки") {
		t.Errorf("status = %+v", s)
	}
}

// Output that is not the expected JSON must be reported as a format change,
// with the command that captures it — not as a generic failure.
func TestProbeReportsAFormatChange(t *testing.T) {
	l, _ := loopEnv(t, goalRollout(t), "not json at all", 0)

	err := l.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "формат событий") {
		t.Fatalf("err = %v, want a format-change diagnosis", err)
	}
	if !strings.Contains(err.Error(), "-raw") {
		t.Error("the diagnosis does not say how to capture the stream")
	}
}

// Hitting the limit during the self-check is not a failure: being told "no"
// proves the machinery works.
func TestProbeTreatsAUsageLimitAsSuccess(t *testing.T) {
	l, _ := loopEnv(t, goalRollout(t),
		`{"type":"turn.failed","error":{"message":"You have hit your usage limit. Try again later."}}`, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := l.Run(ctx); err != nil {
		t.Fatalf("a usage limit during the probe was treated as a failure: %v", err)
	}
}

// The whole point of the daemon: after a turn that hit the limit, it waits
// instead of hammering.
func TestLoopWaitsAfterHittingTheLimit(t *testing.T) {
	resetLocal := time.Now().Add(3 * time.Hour).Format("3:04 PM")
	l, _ := loopEnv(t, goalRollout(t),
		`{"type":"thread.started"}`+"\n"+
			`{"type":"turn.failed","error":{"message":"You have hit your usage limit. Try again at `+resetLocal+`."}}`, 1)
	l.ReadOnlyProbe = false

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = l.Run(ctx); close(done) }()

	// Observed while the daemon is alive: the state after it stops is
	// "stopped", which would say nothing about what happened during the run.
	deadline := time.Now().Add(3 * time.Second)
	var s Status
	for time.Now().Before(deadline) {
		if got, ok := ReadStatus(); ok && got.State == StateLimit {
			s = got
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if s.State != StateLimit {
		t.Fatalf("the daemon never reached the limit wait, last state %q", l.Status().State)
	}
	if s.Limits == 0 {
		t.Error("hitting the limit was not recorded")
	}
	if s.Until.IsZero() {
		t.Error("the reset time from the failure event was not kept")
	}
	if !strings.Contains(s.Line(), "ждёт сброса лимита") {
		t.Errorf("Line = %q", s.Line())
	}
}

// A rollout touched a moment ago means a human is in that thread.
func TestLoopYieldsToAHuman(t *testing.T) {
	l, dir := loopEnv(t, goalRollout(t), `{"type":"turn.completed"}`, 0)
	l.ReadOnlyProbe = false

	now := time.Now()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		os.Chtimes(filepath.Join(dir, e.Name()), now, now)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = l.Run(ctx)

	if l.current.State != StateYielding && l.current.State != StateStopped {
		t.Errorf("state = %q, want the loop to stand down", l.current.State)
	}
	if l.current.Turns != 0 {
		t.Errorf("turns = %d, want 0 while a human is working", l.current.Turns)
	}
}

func TestStatusFileRoundTrip(t *testing.T) {
	setCacheDir(t, t.TempDir())
	w, err := NewStatusWriter()
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	w.Put(Status{State: StateLimit, Until: until, Turns: 3, PID: os.Getpid()})

	got, ok := ReadStatus()
	if !ok {
		t.Fatal("status not readable")
	}
	if got.State != StateLimit || got.Turns != 3 || !got.Until.Equal(until) {
		t.Errorf("got %+v", got)
	}
	if !strings.Contains(got.Line(), "ждёт сброса лимита") {
		t.Errorf("Line = %q", got.Line())
	}

	w.Remove()
	if _, ok := ReadStatus(); ok {
		t.Error("a stopped daemon left its status behind")
	}
}

// A status file left by a process that has died is a leftover, not a state.
func TestStatusOfADeadProcessIsReportedAsStopped(t *testing.T) {
	setCacheDir(t, t.TempDir())
	w, _ := NewStatusWriter()
	w.Put(Status{State: StateRunning, PID: 999999})

	got, ok := ReadStatus()
	if !ok {
		t.Fatal("status not readable")
	}
	if got.State != StateStopped {
		t.Errorf("state = %q, want stopped for a vanished process", got.State)
	}
}
