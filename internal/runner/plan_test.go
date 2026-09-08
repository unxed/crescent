package runner

import (
	"os"
	"path/filepath"
	"runtime"
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

// A machine with daily Codex use and 331 rollout files still had no `codex` on
// PATH: the installers put it in a user-local bin, an npm or nvm prefix, or the
// desktop app's own directory. Failing on PATH alone was wrong.
// codexFile is the name the CLI would actually have on this platform. Windows
// decides "can this be run?" by extension, so a file called plainly "codex" is
// not a program there.
func codexFile() string {
	if runtime.GOOS == "windows" {
		return "codex.exe"
	}
	return "codex"
}

func TestFindCodexHonoursExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, codexFile())
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvCodex, bin)

	got, tried := FindCodex()
	if got != bin {
		t.Errorf("FindCodex = %q, want the override %q", got, bin)
	}
	if len(tried) == 0 {
		t.Error("the searched locations were not reported")
	}
}

func TestFindCodexReportsWhereItLooked(t *testing.T) {
	t.Setenv(EnvCodex, "")
	t.Setenv("PATH", t.TempDir()) // guaranteed to contain no codex
	t.Setenv("HOME", t.TempDir())

	// The system paths are real, and on a machine with the desktop app
	// installed one of them holds an actual codex. Emptying them is what makes
	// "an empty environment" mean anything.
	savedC, savedR := systemCandidates, systemAppRoots
	systemCandidates, systemAppRoots = nil, nil
	defer func() { systemCandidates, systemAppRoots = savedC, savedR }()

	got, tried := FindCodex()
	if got != "" {
		t.Fatalf("found %q in an empty environment", got)
	}
	if len(tried) < 2 {
		t.Fatalf("only %d locations reported; a failure must say where it looked", len(tried))
	}
	joined := strings.Join(tried, "\n")
	// The places worth looking are platform-specific, so the expectations are
	// too: asserting Unix paths on a Windows runner said nothing about the
	// code and everything about the assertion.
	want := []string{"PATH", filepath.Join(".local", "bin", "codex")}
	if runtime.GOOS == "windows" {
		want = []string{"PATH", "OpenAI"}
	}
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("did not try %s:\n%s", w, joined)
		}
	}
}

// A file of the right name that cannot be run must not be mistaken for the CLI.
// What "cannot be run" means differs: a missing execute bit on Unix, a name
// with no executable extension on Windows.
func TestFindCodexIgnoresNonExecutable(t *testing.T) {
	dir := t.TempDir()
	name := "codex"
	if runtime.GOOS == "windows" {
		name = "codex.txt"
	}
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvCodex, bin)
	if got, _ := FindCodex(); got != "" {
		t.Errorf("FindCodex = %q, want empty for a file that cannot be run", got)
	}
}

// And one that can be run must be accepted.
func TestFindCodexAcceptsAnExecutable(t *testing.T) {
	dir := t.TempDir()
	name := "codex"
	if runtime.GOOS == "windows" {
		name = "codex.exe"
	}
	bin := filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvCodex, bin)
	if got, _ := FindCodex(); got != bin {
		t.Errorf("FindCodex = %q, want %q", got, bin)
	}
}

// The ChatGPT desktop app bundles the Codex CLI inside its own installation,
// and there is no standalone `codex` anywhere: on a real machine `whereis
// codex` came back empty while /usr/lib/chatgpt was present. The layout is
// undocumented and versioned, so the binary is found by walking, not guessing.
func TestFindCodexWalksAnAppInstallation(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "resources", "bin", "0.153.0-alpha.5")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(deep, codexFile())
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := searchUnder(root, codexFile(), 7); got != bin {
		t.Errorf("searchUnder = %q, want %q", got, bin)
	}
}

// Bounding the walk is what keeps "look inside the app" from turning into a
// scan of the whole disk.
func TestSearchUnderRespectsDepthLimit(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c", "d", "e", "f", "g", "h")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, codexFile()), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := searchUnder(root, codexFile(), 3); got != "" {
		t.Errorf("searchUnder ignored the depth limit and returned %q", got)
	}
}

// A per-version directory must yield the newest build, not the alphabetically
// first one.
func TestNewestMatchPrefersLaterVersions(t *testing.T) {
	root := t.TempDir()
	for _, v := range []string{"0.144.0", "0.153.0", "0.147.0"} {
		d := filepath.Join(root, v)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, codexFile()), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := newestMatch(filepath.Join(root, "*", codexFile()))
	if len(got) != 3 {
		t.Fatalf("matches = %d, want 3", len(got))
	}
	if !strings.Contains(got[0], "0.153.0") {
		t.Errorf("first match = %s, want the newest version", got[0])
	}
}

// Running the Windows build under Wine showed the cache paths coming out as
// "C:\users\me/.cache/go-build": joined with the wrong separator and following
// the Unix layout. Neither is a path Windows would accept.
func TestDefaultGoCachesFollowThePlatform(t *testing.T) {
	t.Setenv("GOMODCACHE", "")
	t.Setenv("GOCACHE", "")

	got := defaultGoCaches()
	if len(got) != 2 {
		t.Fatalf("got %v, want a module cache and a build cache", got)
	}
	for _, p := range got {
		if !filepath.IsAbs(p) {
			t.Errorf("%q is not absolute", p)
		}
		// filepath.Join is what keeps this true on every platform; string
		// concatenation is what broke it.
		if p != filepath.Clean(p) {
			t.Errorf("%q is not a clean path", p)
		}
		if runtime.GOOS == "windows" && strings.Contains(p, "/") {
			t.Errorf("%q mixes separators on Windows", p)
		}
	}
	if runtime.GOOS == "windows" && strings.Contains(got[1], ".cache") {
		t.Errorf("build cache %q uses the Unix location", got[1])
	}
}

// The environment wins when it is set: a developer who moved their caches must
// not have them silently ignored.
func TestGoCachesPrefersTheEnvironment(t *testing.T) {
	mod := filepath.Join(t.TempDir(), "mod")
	build := filepath.Join(t.TempDir(), "build")
	t.Setenv("GOMODCACHE", mod)
	t.Setenv("GOCACHE", build)

	got := GoCaches()
	if len(got) != 2 || got[0] != mod || got[1] != build {
		t.Errorf("got %v, want [%s %s]", got, mod, build)
	}
}
