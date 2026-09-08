package runner

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/unxed/crescent/internal/codex"
)

func goals() []codex.Session {
	now := time.Now()
	return Goals([]codex.Session{
		{ID: "01a04e32-e089-7e72-83db-7b7aea4714fd", Objective: "Продолжай задачу по плану CONPTYRECONCILE", Status: "paused", Modified: now.Add(-200 * time.Hour)},
		{ID: "01a07e20-c01c-7f71-ac2f-744e11c1f1d8", Objective: "Проект Лунобот-2", Status: "active", Modified: now.Add(-1 * time.Hour)},
		{ID: "01a077b3-6ac6-76d3-b9f4-d5edc26d6c31", Objective: "Молодец! Отличная работа", Status: "paused", Modified: now.Add(-20 * time.Hour)},
		{Objective: "", Status: "", Modified: now}, // no goal: must not appear
	})
}

func TestGoalsOrderIsStableAndFreshestFirst(t *testing.T) {
	g := goals()
	if len(g) != 3 {
		t.Fatalf("goals = %d, want 3 (the goal-less session must be dropped)", len(g))
	}
	if !strings.Contains(g[0].Objective, "Лунобот") {
		t.Errorf("first = %q, want the freshest", g[0].Objective)
	}
	// The number printed by -list is typed into -run-once, so both must derive
	// it from this one function.
	again := goals()
	for i := range g {
		if g[i].ID != again[i].ID {
			t.Fatalf("ordering is not stable at %d", i)
		}
	}
}

func TestResolveByNumber(t *testing.T) {
	g := goals()
	got, err := Resolve(g, "2")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != g[1].ID {
		t.Errorf("got %s, want the second entry", got.ID)
	}
	if _, err := Resolve(g, "99"); err == nil {
		t.Error("a number outside the list was accepted")
	}
}

func TestResolveByIDPrefix(t *testing.T) {
	g := goals()
	got, err := Resolve(g, "01a07e20")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Objective, "Лунобот") {
		t.Errorf("got %q", got.Objective)
	}
}

// Every id here starts with 01a0, so a short prefix must be reported as
// ambiguous rather than silently resolving to whichever came first.
func TestResolveRejectsAmbiguousPrefix(t *testing.T) {
	_, err := Resolve(goals(), "01a0")
	var amb ErrAmbiguous
	if !asAmbiguous(err, &amb) {
		t.Fatalf("err = %v, want ErrAmbiguous", err)
	}
	if len(amb.Matches) != 3 {
		t.Errorf("matches = %d, want 3", len(amb.Matches))
	}
}

// What you remember about a goal is what it was about, not its uuid.
func TestResolveByGoalText(t *testing.T) {
	got, err := Resolve(goals(), "лунобот")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "01a07e20-c01c-7f71-ac2f-744e11c1f1d8" {
		t.Errorf("got %s", got.ID)
	}
}

func TestPickReadsANumber(t *testing.T) {
	var out bytes.Buffer
	got, err := Pick(strings.NewReader("3\n"), &out, goals(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != goals()[2].ID {
		t.Errorf("got %s", got.ID)
	}
	if !strings.Contains(out.String(), "Какую цель продолжить?") {
		t.Error("the prompt was not shown")
	}
}

func TestPickCancelsOnEmptyInput(t *testing.T) {
	var out bytes.Buffer
	if _, err := Pick(strings.NewReader("\n"), &out, goals(), nil); err == nil {
		t.Fatal("an empty answer was treated as a choice")
	}
}

// Without a terminal the picker must fail with advice, not hang or panic.
func TestPickWithoutInputExplainsItself(t *testing.T) {
	var out bytes.Buffer
	_, err := Pick(strings.NewReader(""), &out, goals(), nil)
	if err == nil || !strings.Contains(err.Error(), "аргумент") {
		t.Fatalf("err = %v, want advice to pass the goal as an argument", err)
	}
}

func TestListMarksDeadWorkspaces(t *testing.T) {
	var out bytes.Buffer
	live := func(s codex.Session) bool { return strings.Contains(s.Objective, "Лунобот") }
	WriteList(&out, goals(), live)

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !strings.Contains(lines[0], "Лунобот") || strings.HasPrefix(lines[0], "✗") {
		t.Errorf("the live workspace was marked dead: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "✗") {
		t.Errorf("a vanished workspace was not marked: %q", lines[1])
	}
}

func asAmbiguous(err error, target *ErrAmbiguous) bool {
	if e, ok := err.(ErrAmbiguous); ok {
		*target = e
		return true
	}
	return false
}
