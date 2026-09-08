package codex

import (
	"strings"
	"testing"
)

// The point of Find: the user knows the chat is called "Konsole", so searching
// for that value tells us which key holds it. Guessing key names produced
// "exec" and "auto" on real data.
func TestFindReportsTheKeyPathOfAKnownValue(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "rollout-1.jsonl",
		`{"type":"session_meta","payload":{"metadata":{"display_name":"Konsole"}}}`+"\n"+
			`{"type":"turn","text":"обычный текст"}`)

	hits, err := Find(dir, "Konsole", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("nothing found")
	}
	var paths []string
	for _, h := range hits {
		paths = append(paths, h.KeyPath)
	}
	joined := strings.Join(paths, ", ")
	if !strings.Contains(joined, "payload.metadata.display_name") {
		t.Errorf("key path not reported, got: %s", joined)
	}
}

func TestFindIsCaseInsensitiveAndReportsLine(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "rollout-1.jsonl",
		`{"a":"nothing"}`+"\n"+`{"b":"nothing"}`+"\n"+`{"chat_title":"Лунобот-1, 4656"}`)

	hits, err := Find(dir, "лунобот-1, 4656", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if hits[0].Line != 3 {
		t.Errorf("Line = %d, want 3", hits[0].Line)
	}
	if hits[0].KeyPath != "chat_title" {
		t.Errorf("KeyPath = %q", hits[0].KeyPath)
	}
}

// Nested arrays must be addressable too, otherwise a value inside a list of
// items would be reported without a usable path.
func TestFindWalksArrays(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "rollout-1.jsonl", `{"items":[{"k":"no"},{"k":"Konsole"}]}`)

	hits, err := Find(dir, "Konsole", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].KeyPath, "items[1].k") {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestFindRefusesAnEmptyNeedle(t *testing.T) {
	if _, err := Find(t.TempDir(), "   ", 10); err == nil {
		t.Error("an empty search was accepted")
	}
}

// The regression this whole change exists for: an active goal sat at line 30255
// of a rollout still being appended to, and reading only the head and tail
// missed it entirely. Long sessions are exactly the ones that hit usage limits.
func TestGoalDeepInsideAHugeRolloutIsFound(t *testing.T) {
	dir := t.TempDir()

	var b strings.Builder
	b.WriteString(`{"type":"session_meta","cwd":"/tmp/work"}` + "\n")
	filler := `{"type":"turn","payload":{"item":{"stdout":"` + strings.Repeat("x", 4000) + `"}}}` + "\n"
	for i := 0; i < 200; i++ { // ~800 KB before the goal
		b.WriteString(filler)
	}
	b.WriteString(`{"type":"goal","payload":{"goal":{"objective":"довести Konsole до запуска","status":"active"}}}` + "\n")
	for i := 0; i < 200; i++ { // ~800 KB after it
		b.WriteString(filler)
	}
	p := write(t, dir, "rollout-2026-08-29T23-07-22-01a04f59-3ac3-7790-9f25-a0bd6c10ca2b.jsonl", b.String())

	s, err := ScanFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasGoal() {
		t.Fatal("goal buried in the middle of the file was not found")
	}
	if s.Objective != "довести Konsole до запуска" {
		t.Errorf("Objective = %q", s.Objective)
	}
	if s.Cwd != "/tmp/work" {
		t.Errorf("Cwd = %q", s.Cwd)
	}
}

// The prefilter must not drop records: it only decides what is worth parsing.
func TestPrefilterKeepsEveryRelevantRecord(t *testing.T) {
	for _, line := range []string{
		`{"payload":{"goal":{"objective":"x"}}}`,
		`{"rate_limits":{"primary":{"resets_at":"2026-01-01T00:00:00Z"}}}`,
		`{"type":"session_meta","cwd":"/x"}`,
		`{"conversation_title":"x"}`,
		`{"payload":{"workingDirectory":"/x"}}`,
	} {
		if !interesting([]byte(line)) {
			t.Errorf("prefilter rejected a relevant record: %s", line)
		}
	}
	// Ordinary tool output carries none of the markers.
	if interesting([]byte(`{"type":"turn","text":"обычный вывод команды"}`)) {
		t.Error("prefilter accepted a line with no markers")
	}
}

// A rollout that has not changed must not be read a second time.
func TestCacheAvoidsRereadingUnchangedFiles(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := t.TempDir()
	write(t, dir, "rollout-1.jsonl", `{"type":"goal","goal":{"objective":"первая","status":"active"}}`)

	first, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Objective != "первая" {
		t.Fatalf("first scan: %+v", first)
	}

	var reread int
	second, err := ScanWithProgress(dir, func(int, int, string) { reread++ })
	if err != nil {
		t.Fatal(err)
	}
	if reread != 0 {
		t.Errorf("unchanged file was read again %d time(s)", reread)
	}
	if len(second) != 1 || second[0].Objective != "первая" {
		t.Errorf("cached result differs from the fresh one: %+v", second)
	}
}

// A file that grew must be re-read, or a goal changed today would be invisible.
func TestCacheNoticesAChangedFile(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := t.TempDir()
	p := write(t, dir, "rollout-1.jsonl", `{"type":"goal","goal":{"objective":"первая","status":"active"}}`)

	if _, err := Scan(dir); err != nil {
		t.Fatal(err)
	}

	write(t, dir, "rollout-1.jsonl",
		`{"type":"goal","goal":{"objective":"первая","status":"active"}}`+"\n"+
			`{"type":"goal","goal":{"objective":"вторая","status":"paused"}}`)
	_ = p

	var reread int
	got, err := ScanWithProgress(dir, func(int, int, string) { reread++ })
	if err != nil {
		t.Fatal(err)
	}
	if reread != 1 {
		t.Errorf("changed file was re-read %d time(s), want 1", reread)
	}
	if got[0].Objective != "вторая" {
		t.Errorf("Objective = %q, want the updated goal", got[0].Objective)
	}
}
