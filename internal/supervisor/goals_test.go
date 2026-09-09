package supervisor

import (
	"sort"
	"testing"

	"github.com/unxed/crescent/internal/appserver"
)

// dedupe mirrors what Goals does, so the rule can be checked without a server.
// Extracted here because the rule itself is what matters: forks carry the name
// and objective of the thread they came from, so the same goal appeared four
// times and pushed the goals that mattered off the visible list.
func dedupe(cands []appserver.Candidate) []GoalView {
	best := map[string]appserver.Candidate{}
	var order []string
	for _, c := range cands {
		key := c.Thread.Label() + "\x00" + c.Goal.Objective
		prev, seen := best[key]
		if !seen {
			order = append(order, key)
			best[key] = c
			continue
		}
		if c.Thread.Updated > prev.Thread.Updated {
			best[key] = c
		}
	}
	out := make([]GoalView, 0, len(order))
	for _, k := range order {
		c := best[k]
		out = append(out, GoalView{ThreadID: c.Thread.ID, Label: c.Thread.Label(),
			Status: c.Goal.Status, Updated: c.Thread.Updated})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out
}

func cand(id, name, objective string, updated int64) appserver.Candidate {
	return appserver.Candidate{
		Thread: appserver.Thread{ID: id, Name: name, Updated: updated},
		Goal:   appserver.Goal{Objective: objective, Status: "paused"},
	}
}

// Four forks of one goal must collapse to one row, and the survivor must be the
// freshest — driving a stale fork would spend the quota on a dead branch.
func TestForksCollapseToTheFreshest(t *testing.T) {
	got := dedupe([]appserver.Candidate{
		cand("a", "f4 SDL в Alpine", "Обновись из репозитория", 100),
		cand("b", "f4 SDL в Alpine", "Обновись из репозитория", 300),
		cand("c", "f4 SDL в Alpine", "Обновись из репозитория", 200),
		cand("d", "Konsole", "довести Konsole", 400),
	})
	if len(got) != 2 {
		t.Fatalf("осталось %d строк, ожидалось 2: %+v", len(got), got)
	}
	for _, g := range got {
		if g.Label == "f4 SDL в Alpine" && g.ThreadID != "b" {
			t.Errorf("выжил не самый свежий форк: %s", g.ThreadID)
		}
	}
}

// Same name, different objective, is a different goal and must not be merged:
// collapsing it would silently drop work the user pinned.
func TestSameNameDifferentGoalSurvives(t *testing.T) {
	got := dedupe([]appserver.Candidate{
		cand("a", "Следовать инструкции", "Ты — Лунобот-1", 100),
		cand("b", "Следовать инструкции", "Ты — Лунобот-2", 200),
	})
	if len(got) != 2 {
		t.Fatalf("две разные цели схлопнулись в %d", len(got))
	}
}

// Freshest first: the goal touched last is the one being thought about, and
// burying it below stale ones is how the Луноботы ended up out of sight.
func TestFreshestGoalComesFirst(t *testing.T) {
	got := dedupe([]appserver.Candidate{
		cand("old", "Старая", "цель", 100),
		cand("new", "Свежая", "цель", 900),
	})
	if got[0].ThreadID != "new" {
		t.Errorf("сверху %q, ожидалась самая свежая", got[0].Label)
	}
}
