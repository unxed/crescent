package codex

import (
	"os"
	"path/filepath"
	"strings"
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
	// Asserted against the recorded windows rather than ResetsAt(), which
	// filters by the current time: a fixture with a pinned date would quietly
	// start failing once that date passes.
	want := time.Date(2026, 9, 8, 9, 30, 0, 0, time.UTC)
	found := false
	for _, r := range s.Resets {
		if r.At.Equal(want) {
			found = true
		}
	}
	if !found {
		t.Errorf("Resets = %v, want one at %v", s.Resets, want)
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
	if len(s.Resets) != 1 || s.Resets[0].At.UTC().Year() != 2026 {
		t.Errorf("Resets = %v (epoch millis misread?)", s.Resets)
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

// The cases below all come from a real ~/.codex/sessions directory (331 files).
// Each one is a bug the synthetic fixtures could not have shown.

func TestCwdFileURLIsNormalised(t *testing.T) {
	cases := map[string]string{
		"/plain/path":        "/plain/path",
		"file:///tmp/f4-885": "/tmp/f4-885",
		// A cwd with non-ASCII characters arrives percent-encoded; passing it
		// to a process unchanged would fail.
		"file:///home/unxed/%D0%94%D0%BE%D0%BA%D1%83%D0%BC%D0%B5%D0%BD%D1%82%D1%8B/ChatGPT/f4": "/home/unxed/Документы/ChatGPT/f4",
	}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Two rollouts continued from the same parent thread must not report the same
// session id: `rollout-<ts>-<parent>_<own>.jsonl`.
func TestForkedRolloutsGetDistinctIDs(t *testing.T) {
	dir := t.TempDir()
	body := `{"type":"turn"}`
	a := write(t, dir, "rollout-2026-08-31T02-18-27-01a0552d-0082-75b0-9a07-e6fa64943c5f_01a0552e-8698-7961-b9ab-199d982579a2.jsonl", body)
	b := write(t, dir, "rollout-2026-09-01T06-47-40-01a0552d-0082-75b0-9a07-e6fa64943c5f_01a05b4b-5ab6-7020-8e33-806d364f6f35.jsonl", body)

	sa, _ := ScanFile(a)
	sb, _ := ScanFile(b)
	if sa.ID == sb.ID {
		t.Fatalf("both rollouts claim id %s", sa.ID)
	}
	if sa.ID != "01a0552e-8698-7961-b9ab-199d982579a2" {
		t.Errorf("ID = %q, want the rollout's own uuid, not the parent's", sa.ID)
	}
	if sa.ParentID != "01a0552d-0082-75b0-9a07-e6fa64943c5f" {
		t.Errorf("ParentID = %q", sa.ParentID)
	}
}

func TestInFileIDBeatsFilename(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "rollout-2026-09-08T01-00-48-01a07e1a-4cfb-7e60-a893-05507286e475.jsonl",
		`{"type":"session_meta","id":"01a07e20-c01c-7f71-ac2f-744e11c1f1d8"}`)
	s, _ := ScanFile(p)
	if s.ID != "01a07e20-c01c-7f71-ac2f-744e11c1f1d8" {
		t.Errorf("ID = %q, want the id recorded inside the file", s.ID)
	}
	if s.IDKey == "filename" {
		t.Error("IDKey still says filename")
	}
}

// A rollout carries both the rolling and the weekly window. Picking whichever
// happened to be written last is a coin flip; the nearest future one is what
// scheduling needs.
func TestBothLimitWindowsAreKeptAndNearestFutureWins(t *testing.T) {
	soon := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	late := time.Now().Add(160 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-40 * time.Hour).UTC().Format(time.RFC3339)

	dir := t.TempDir()
	p := write(t, dir, "rollout-1.jsonl",
		`{"rate_limits":{"secondary":{"resets_at":"`+late+`"},"primary":{"resets_at":"`+soon+`"}}}`+"\n"+
			`{"rate_limits":{"primary":{"resets_at":"`+past+`"}}}`)

	s, _ := ScanFile(p)
	if len(s.Resets) != 3 {
		t.Fatalf("kept %d windows, want 3", len(s.Resets))
	}
	got := s.ResetsAt()
	if d := time.Until(got); d < 2*time.Hour || d > 4*time.Hour {
		t.Errorf("ResetsAt = %v (in %v), want the ~3h window", got, d.Round(time.Minute))
	}
}

// Every window already lapsed: the session says nothing about now.
func TestLapsedWindowsReportNothing(t *testing.T) {
	past := time.Now().Add(-200 * time.Hour).UTC().Format(time.RFC3339)
	dir := t.TempDir()
	p := write(t, dir, "rollout-1.jsonl", `{"rate_limits":{"resets_at":"`+past+`"}}`)
	s, _ := ScanFile(p)
	if !s.ResetsAt().IsZero() {
		t.Errorf("ResetsAt = %v, want zero for a fully lapsed snapshot", s.ResetsAt())
	}
}

// The account limit comes from the freshest rollout, not from whichever session
// the caller happens to be looking at.
func TestAccountLimitsUsesFreshestRollout(t *testing.T) {
	soon := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	stale := time.Now().Add(-100 * time.Hour).UTC().Format(time.RFC3339)

	dir := t.TempDir()
	oldP := write(t, dir, "rollout-old.jsonl", `{"rate_limits":{"resets_at":"`+stale+`"}}`)
	write(t, dir, "rollout-new.jsonl", `{"rate_limits":{"resets_at":"`+soon+`"}}`)
	old := time.Now().Add(-100 * time.Hour)
	if err := os.Chtimes(oldP, old, old); err != nil {
		t.Fatal(err)
	}

	sessions, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	from, windows, ok := AccountLimits(sessions)
	if !ok {
		t.Fatal("no account limits found")
	}
	if filepath.Base(from.Path) != "rollout-new.jsonl" {
		t.Errorf("limits taken from %s, want the freshest rollout", filepath.Base(from.Path))
	}
	if len(windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(windows))
	}
	if d := time.Until(NextReset(sessions)); d < time.Hour || d > 3*time.Hour {
		t.Errorf("NextReset in %v, want ~2h", d.Round(time.Minute))
	}
}

// Objectives are free-form user text: multi-line and markdown-escaped.
func TestTitleIsSingleLineAndUnescaped(t *testing.T) {
	s := Session{Objective: "Продолжай задачу по плану docs/CONPTYRECONCILE\\_PLAN.md.\n\nСначала обновись."}
	got := s.Title()
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("Title contains a newline: %q", got)
	}
	if strings.Contains(got, "\\_") {
		t.Errorf("Title kept markdown escaping: %q", got)
	}
	if !strings.Contains(got, "CONPTYRECONCILE_PLAN.md") {
		t.Errorf("Title = %q", got)
	}
}

func TestObjectiveProvenanceIsRecorded(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "rollout-1.jsonl", `{"goal":{"objective":"починить CI","status":"active"}}`)
	s, _ := ScanFile(p)
	if s.ObjectiveKey != "goal.objective" {
		t.Errorf("ObjectiveKey = %q, want goal.objective", s.ObjectiveKey)
	}
}
