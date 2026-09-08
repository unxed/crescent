package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Chat names are not in the rollouts. The reverse search found them under
// `thread_name` in session_index.jsonl, beside the sessions directory.
func TestChatNamesComeFromTheSessionIndex(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, IndexFile), []byte(
		`{"id":"01a08170-c01c-7f71-ac2f-744e11c1f1d8","thread_name":"Лунобот-1, 4656"}`+"\n"+
			`{"id":"01a04f59-3ac3-7790-9f25-a0bd6c10ca2b","thread_name":"Konsole"}`+"\n"+
			`{"broken`), 0o644); err != nil {
		t.Fatal(err)
	}

	idx := LoadIndex(home)
	if len(idx) != 2 {
		t.Fatalf("loaded %d entries, want 2 (the corrupt line must be skipped)", len(idx))
	}
	if idx["01a04f59-3ac3-7790-9f25-a0bd6c10ca2b"].Name != "Konsole" {
		t.Errorf("got %q", idx["01a04f59-3ac3-7790-9f25-a0bd6c10ca2b"].Name)
	}

	sessions := []Session{
		{ID: "01a04f59-3ac3-7790-9f25-a0bd6c10ca2b", Objective: "довести Konsole до запуска"},
		{ID: "unknown-session", Objective: "без имени"},
	}
	applyIndex(sessions, idx)

	if sessions[0].Name != "Konsole" {
		t.Errorf("Name = %q", sessions[0].Name)
	}
	if sessions[0].NameKey != IndexFile {
		t.Errorf("NameKey = %q, want the index file", sessions[0].NameKey)
	}
	if !strings.HasPrefix(sessions[0].Label(), "Konsole — довести Konsole") {
		t.Errorf("Label = %q", sessions[0].Label())
	}
	if sessions[1].Name != "" {
		t.Errorf("a session missing from the index got a name: %q", sessions[1].Name)
	}
}

// A forked rollout carries its own id; the index may only know the thread it
// was continued from.
func TestNameFallsBackToTheParentThread(t *testing.T) {
	idx := map[string]IndexEntry{"01a0552d-0082-75b0-9a07-e6fa64943c5f": {Name: "Почини CI в main"}}
	sessions := []Session{{
		ID:       "01a0552e-8698-7961-b9ab-199d982579a2",
		ParentID: "01a0552d-0082-75b0-9a07-e6fa64943c5f",
	}}
	applyIndex(sessions, idx)
	if sessions[0].Name != "Почини CI в main" {
		t.Errorf("Name = %q", sessions[0].Name)
	}
}

// No index at all must be harmless: names are a nicety.
func TestMissingIndexIsNotAnError(t *testing.T) {
	idx := LoadIndex(t.TempDir())
	if len(idx) != 0 {
		t.Fatalf("got %d entries from a directory with no index", len(idx))
	}
	sessions := []Session{{ID: "x", Objective: "цель"}}
	applyIndex(sessions, idx)
	if sessions[0].Label() != "цель" {
		t.Errorf("Label = %q", sessions[0].Label())
	}
}

func TestIndexKeysReportsNamesOnly(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, IndexFile), []byte(
		`{"id":"01a08170-c01c-7f71-ac2f-744e11c1f1d8","thread_name":"Лунобот-1, 4656","updated_at":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := IndexKeys(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id", "thread_name", "updated_at"} {
		if keys[want] == 0 {
			t.Errorf("key %q not reported", want)
		}
	}
	for k := range keys {
		if strings.Contains(k, "Лунобот") {
			t.Fatalf("IndexKeys leaked a value: %q", k)
		}
	}
}
