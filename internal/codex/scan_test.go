package codex

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The rollout schema is undocumented and moves. These fixtures are not claims
// about what Codex writes today — they are the shapes the parser promises to
// survive. When -dump shows a real file that none of them matches, the fixture
// set grows and this test tells us whether the recogniser still works.

const flatRecords = `{"type":"session_meta","id":"019f78cf-a24c-7f30-80ac-c32b3ccfc887","cwd":"/home/u/proj"}
{"type":"goal","goal":{"objective":"починить флаки в CI","status":"active"}}
{"type":"turn","tokens":1234}
{"type":"rate_limits","rate_limits":{"resets_at":"2026-09-08T09:30:00Z","limit_name":"codex"}}
{"type":"goal","goal":{"objective":"починить флаки в CI","status":"usageLimited"}}`

// Same information, different spellings and nesting: camelCase keys, the goal
// as a bare string, the reset as epoch milliseconds inside a deeper object.
const camelRecords = `{"payload":{"threadId":"019f78cf-a24c-7f30-80ac-c32b3ccfc888","workingDirectory":"/srv/app"}}
{"payload":{"goal":"дописать тесты бэкенда","goalStatus":"paused"}}
{"payload":{"usage":{"rateLimits":{"resetsAt":1789000000000}}}}`

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScanFileFlatSchema(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "rollout-2026-09-08T08-00-00-019f78cf-a24c-7f30-80ac-c32b3ccfc887.jsonl", flatRecords)

	s, err := ScanFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "019f78cf-a24c-7f30-80ac-c32b3ccfc887" {
		t.Errorf("ID = %q", s.ID)
	}
	if s.Cwd != "/home/u/proj" {
		t.Errorf("Cwd = %q", s.Cwd)
	}
	if s.Objective != "починить флаки в CI" {
		t.Errorf("Objective = %q", s.Objective)
	}
	// The later record must win: the goal ended up usage-limited, not active.
	if s.Status != StatusUsageLimited {
		t.Errorf("Status = %q, want usageLimited", s.Status)
	}
	want := time.Date(2026, 9, 8, 9, 30, 0, 0, time.UTC)
	if !s.ResetsAt.Equal(want) {
		t.Errorf("ResetsAt = %v, want %v", s.ResetsAt, want)
	}
	if !s.HasGoal() {
		t.Error("HasGoal = false")
	}
	if !s.Status.Resumable() {
		t.Error("a usage-limited goal must be resumable — that is the whole point")
	}
}

func TestScanFileCamelSchema(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "rollout-x.jsonl", camelRecords)

	s, err := ScanFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "019f78cf-a24c-7f30-80ac-c32b3ccfc888" {
		t.Errorf("ID = %q", s.ID)
	}
	if s.Cwd != "/srv/app" {
		t.Errorf("Cwd = %q", s.Cwd)
	}
	if s.Objective != "дописать тесты бэкенда" {
		t.Errorf("Objective = %q", s.Objective)
	}
	if s.Status != "paused" {
		t.Errorf("Status = %q", s.Status)
	}
	if got := s.ResetsAt.UTC().Year(); got != 2026 {
		t.Errorf("ResetsAt = %v (epoch millis misread?)", s.ResetsAt.UTC())
	}
}

func TestCompletedGoalIsNotResumable(t *testing.T) {
	for _, st := range []GoalStatus{"complete", "Complete", "COMPLETED", "blocked"} {
		if st.Resumable() {
			t.Errorf("%q must not be resumable", st)
		}
	}
	for _, st := range []GoalStatus{"active", "paused", "usageLimited", "somethingNew"} {
		if !st.Resumable() {
			t.Errorf("%q must be resumable — unknown states default to retryable", st)
		}
	}
}

func TestScanSkipsJunkAndSortsByRecency(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a/rollout-1.jsonl", flatRecords)
	write(t, dir, "b/rollout-2.jsonl", camelRecords)
	write(t, dir, "b/broken.jsonl", "{not json at all\n\x00\x01")
	write(t, dir, "b/notes.txt", "ignored")

	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "a/rollout-1.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}

	got, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("scanned %d files, want 3 (.jsonl only, corrupt one still listed)", len(got))
	}
	if filepath.Base(got[len(got)-1].Path) != "rollout-1.jsonl" {
		t.Errorf("oldest file is not last: %v", got[len(got)-1].Path)
	}
	// A corrupt file must appear but claim nothing.
	for _, s := range got {
		if filepath.Base(s.Path) == "broken.jsonl" && s.HasGoal() {
			t.Error("corrupt file reported a goal")
		}
	}
}

func TestEpochUnitsAreGuessedCorrectly(t *testing.T) {
	ref := time.Date(2026, 9, 8, 9, 30, 0, 0, time.UTC)
	cases := map[string]float64{
		"seconds":      float64(ref.Unix()),
		"milliseconds": float64(ref.UnixMilli()),
		"microseconds": float64(ref.UnixMicro()),
		"nanoseconds":  float64(ref.UnixNano()),
	}
	for name, v := range cases {
		if got := epochToTime(v); got.UTC().Truncate(time.Second) != ref {
			t.Errorf("%s: got %v, want %v", name, got.UTC(), ref)
		}
	}
}

func TestKeysCollectsNamesOnly(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "rollout-1.jsonl", flatRecords)

	keys, err := Keys(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cwd", "goal", "objective", "status", "resets_at"} {
		if keys[want] == 0 {
			t.Errorf("key %q not collected", want)
		}
	}
	// The point of Keys is that a report can be pasted publicly: no value from
	// the file may appear among the collected names.
	for k := range keys {
		if k == "починить флаки в CI" || k == "/home/u/proj" {
			t.Fatalf("Keys leaked a value: %q", k)
		}
	}
}
