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

// A computer should not ask a human to remember a number it just printed:
// Enter takes the offered goal.
func TestPickAcceptsDefaultOnEnter(t *testing.T) {
	var out bytes.Buffer
	got, err := Pick(strings.NewReader("\n"), &out, goals(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Objective, "Лунобот") {
		t.Errorf("got %q, want the freshest runnable goal", got.Objective)
	}
	if !strings.Contains(out.String(), "Продолжить цель 1") {
		t.Errorf("the offer was not shown:\n%s", out.String())
	}
}

func TestPickNumberOverridesTheDefault(t *testing.T) {
	var out bytes.Buffer
	got, err := Pick(strings.NewReader("3\n"), &out, goals(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != goals()[2].ID {
		t.Errorf("got %s", got.ID)
	}
}

func TestPickCancelsOnQ(t *testing.T) {
	var out bytes.Buffer
	if _, err := Pick(strings.NewReader("q\n"), &out, goals(), nil); err == nil {
		t.Fatal("q did not cancel")
	}
}

// The offered goal must be one that can actually run: an unusable one would
// turn a single keystroke into a wasted turn.
func TestDefaultSkipsWhatCannotRun(t *testing.T) {
	g := goals()
	live := func(s codex.Session) bool { return !strings.Contains(s.Objective, "Лунобот") }

	got, ok := Default(g, live)
	if !ok {
		t.Fatal("no default found")
	}
	if strings.Contains(got.Objective, "Лунобот") {
		t.Error("the default is a goal with a dead workspace")
	}

	var out bytes.Buffer
	picked, err := Pick(strings.NewReader("\n"), &out, g, live)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != got.ID {
		t.Errorf("Enter picked %s, not the offered default %s", picked.ID, got.ID)
	}
}

func TestDefaultIgnoresFinishedGoals(t *testing.T) {
	now := time.Now()
	g := Goals([]codex.Session{
		{ID: "done", Objective: "готово", Status: "complete", Modified: now},
		{ID: "live", Objective: "работа", Status: "paused", Modified: now.Add(-time.Hour)},
	})
	got, ok := Default(g, nil)
	if !ok || got.ID != "live" {
		t.Errorf("default = %v/%v, want the resumable one", got.ID, ok)
	}
}

// Without a terminal the picker must fail with advice, not hang or panic.
func TestPickWithoutInputExplainsItself(t *testing.T) {
	var out bytes.Buffer
	_, err := Pick(strings.NewReader(""), &out, goals(), nil)
	if err == nil || !strings.Contains(err.Error(), "-yes") {
		t.Fatalf("err = %v, want advice about -yes", err)
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
