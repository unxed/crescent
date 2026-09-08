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

	hits, err := Find(dir, "Konsole", false, 50)
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

	hits, err := Find(dir, "лунобот-1, 4656", false, 50)
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

	hits, err := Find(dir, "Konsole", false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].KeyPath, "items[1].k") {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestFindRefusesAnEmptyNeedle(t *testing.T) {
	if _, err := Find(t.TempDir(), "   ", false, 10); err == nil {
		t.Error("an empty search was accepted")
	}
}
