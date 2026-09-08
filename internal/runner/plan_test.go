package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/unxed/crescent/internal/codex"
)

func policy() Policy {
	p := DefaultPolicy()
	p.WritableRoots = []string{"/home/u/go/pkg/mod", "/home/u/.cache/go-build"}
	p.Prompt = "Продолжай."
	return p
}

func TestBuildArgsOrdersFlagsForCodexCLI(t *testing.T) {
	s := codex.Session{ID: "01a0552e-8698-7961-b9ab-199d982579a2", Cwd: "/tmp/work", Objective: "x"}
	got := BuildArgs(s, policy())

	joined := strings.Join(got, " ")
	// exec's own flags have to come before the resume subcommand, and the
	// session id and prompt after it.
	iJSON := indexOf(got, "--json")
	iResume := indexOf(got, "resume")
	iID := indexOf(got, s.ID)
	if iJSON < 0 || iResume < 0 || iID < 0 {
		t.Fatalf("argv missing pieces: %v", got)
	}
	if !(iJSON < iResume && iResume < iID) {
		t.Errorf("wrong order, exec flags must precede resume: %v", got)
	}
	if got[len(got)-1] != "Продолжай." {
		t.Errorf("prompt is not the last argument: %v", got)
	}
	if !strings.Contains(joined, "--cd /tmp/work") {
		t.Errorf("working directory not passed: %v", got)
	}
	if !strings.Contains(joined, "--sandbox workspace-write") {
		t.Errorf("sandbox not passed: %v", got)
	}
}

// The user's rule: stay in the sandbox, but the Go caches must stay writable —
// otherwise every resumed build either re-downloads everything or just fails.
func TestWritableRootsCoverGoCaches(t *testing.T) {
	s := codex.Session{ID: "id", Cwd: "/tmp/work"}
	got := strings.Join(BuildArgs(s, policy()), " ")
	want := `sandbox_workspace_write.writable_roots=["/home/u/go/pkg/mod","/home/u/.cache/go-build"]`
	if !strings.Contains(got, want) {
		t.Errorf("writable roots override missing or malformed:\n got %s\nwant substring %s", got, want)
	}
}

func TestPlanSkipsWhatItCannotRun(t *testing.T) {
	now := time.Now()
	long := now.Add(-48 * time.Hour)
	dir := t.TempDir()

	sessions := []codex.Session{
		{ID: "a", Cwd: dir, Objective: "ok", Status: "active", Modified: long},
		{ID: "b", Cwd: dir, Objective: "done", Status: "complete", Modified: long},
		{ID: "", Cwd: dir, Objective: "no id", Status: "active", Modified: long},
		{ID: "d", Cwd: "/nonexistent/path/xyz", Objective: "gone", Status: "paused", Modified: long},
		{ID: "e", Cwd: dir, Objective: "no goal is filtered out earlier", Modified: long},
	}
	plan := Build(sessions, policy(), now)

	byID := map[string]SkipReason{}
	for _, st := range plan.Steps {
		byID[st.Session.ID] = st.Skip
	}
	if byID["a"] != "" {
		t.Errorf("runnable session was skipped: %q", byID["a"])
	}
	if byID["b"] != SkipNotResumable {
		t.Errorf("completed goal: %q", byID["b"])
	}
	if byID[""] != SkipNoID {
		t.Errorf("session without id: %q", byID[""])
	}
	if byID["d"] != SkipNoCwd {
		t.Errorf("vanished /tmp workspace: %q", byID["d"])
	}
	if n := len(plan.Runnable()); n != 2 {
		t.Errorf("runnable = %d, want 2 (a and e)", n)
	}
}

// If any rollout changed a moment ago, a human is in there. The usage limit is
// shared, so crescent competing for it would be taking work away from the user.
func TestPlanYieldsToActiveHuman(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	sessions := []codex.Session{
		{ID: "a", Cwd: dir, Objective: "ok", Status: "active", Modified: now.Add(-48 * time.Hour)},
		{ID: "b", Cwd: dir, Objective: "busy", Status: "active", Modified: now.Add(-30 * time.Second)},
	}
	plan := Build(sessions, policy(), now)
	if !plan.Interrupt {
		t.Fatal("a rollout written 30s ago did not raise the stand-down flag")
	}
}

// Goals run newest first: the one you touched last is the one you care about.
func TestPlanOrdersFreshestFirst(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	sessions := []codex.Session{
		{ID: "old", Cwd: dir, Objective: "o", Status: "active", Modified: now.Add(-100 * time.Hour)},
		{ID: "new", Cwd: dir, Objective: "n", Status: "active", Modified: now.Add(-2 * time.Hour)},
	}
	plan := Build(sessions, policy(), now)
	if plan.Steps[0].Session.ID != "new" {
		t.Errorf("order = %s first, want the freshest", plan.Steps[0].Session.ID)
	}
}

// A live limit window becomes the plan's start time; nothing runs before it.
func TestPlanWaitsForTheLimitWindow(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	reset := now.Add(3 * time.Hour)
	sessions := []codex.Session{{
		ID: "a", Cwd: dir, Objective: "ok", Status: "active",
		Modified: now.Add(-48 * time.Hour),
		Resets:   []codex.Reset{{At: reset, Key: "resets_at"}},
	}}
	plan := Build(sessions, policy(), now)
	if !plan.Limited {
		t.Fatal("plan does not know the account is limited")
	}
	if !plan.StartAt.Equal(reset) {
		t.Errorf("StartAt = %v, want %v", plan.StartAt, reset)
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
