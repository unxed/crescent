package runner

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/unxed/crescent/internal/codex"
)

// Goals returns the sessions worth showing in a chooser, in the order both the
// list and the selector use. Ordering has to come from one place: a number
// printed by one command and read by another must mean the same session.
func Goals(sessions []codex.Session) []codex.Session {
	var out []codex.Session
	for _, s := range sessions {
		if s.HasGoal() {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

// ErrNoSelection is returned when the user declines to choose.
type ErrNoSelection struct{}

func (ErrNoSelection) Error() string { return "цель не выбрана" }

// ErrAmbiguous lists the sessions a selector matched.
type ErrAmbiguous struct {
	Selector string
	Matches  []codex.Session
}

func (e ErrAmbiguous) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%q подходит сразу нескольким целям:\n", e.Selector)
	for _, m := range e.Matches {
		fmt.Fprintf(&b, "    %s  %s\n", ShortID(m.ID), m.Title())
	}
	b.WriteString("уточните номер или более длинный кусок id")
	return b.String()
}

// ShortID is the id prefix a human can retype without hating you. Codex session
// ids are UUIDs and the first block is already distinctive in practice.
func ShortID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Resolve turns whatever the user typed into one session.
//
// Three forms are accepted, in this order: a position in the list, a prefix of
// the session id, and a fragment of the goal text. The last one exists because
// what you remember about a goal is what it was about, not its uuid.
func Resolve(goals []codex.Session, selector string) (codex.Session, error) {
	sel := strings.TrimSpace(selector)
	if sel == "" {
		return codex.Session{}, ErrNoSelection{}
	}

	if n, err := strconv.Atoi(sel); err == nil {
		if n < 1 || n > len(goals) {
			return codex.Session{}, fmt.Errorf("номер %d вне списка (целей: %d)", n, len(goals))
		}
		return goals[n-1], nil
	}

	low := strings.ToLower(sel)

	var byID []codex.Session
	for _, g := range goals {
		if strings.HasPrefix(strings.ToLower(g.ID), low) {
			byID = append(byID, g)
		}
	}
	switch len(byID) {
	case 1:
		return byID[0], nil
	case 0:
	default:
		return codex.Session{}, ErrAmbiguous{Selector: sel, Matches: byID}
	}

	var byText []codex.Session
	for _, g := range goals {
		if strings.Contains(strings.ToLower(g.Objective), low) {
			byText = append(byText, g)
		}
	}
	switch len(byText) {
	case 1:
		return byText[0], nil
	case 0:
		return codex.Session{}, fmt.Errorf("ничего не найдено по %q", sel)
	default:
		return codex.Session{}, ErrAmbiguous{Selector: sel, Matches: byText}
	}
}

// WriteList prints the numbered chooser. workspaceLive reports whether a
// session's working directory still exists — many were created under /tmp and
// did not survive a reboot, and picking one of those only wastes a turn.
func WriteList(w io.Writer, goals []codex.Session, workspaceLive func(codex.Session) bool) {
	if len(goals) == 0 {
		fmt.Fprintln(w, "Целей не найдено.")
		return
	}
	for i, g := range goals {
		mark := "  "
		if workspaceLive != nil && !workspaceLive(g) {
			mark = "✗ " // workspace gone
		}
		fmt.Fprintf(w, "%s%2d. %-9s %-9s %s\n", mark, i+1, ShortID(g.ID), g.Status, g.Title())
	}
	if workspaceLive != nil {
		fmt.Fprintln(w, "\n✗ — рабочий каталог не существует, продолжать нечего")
	}
}

// Pick shows the list and reads a choice. An empty answer cancels.
func Pick(in io.Reader, out io.Writer, goals []codex.Session, workspaceLive func(codex.Session) bool) (codex.Session, error) {
	WriteList(out, goals, workspaceLive)
	fmt.Fprintf(out, "\nКакую цель продолжить? [1-%d, Enter — отмена]: ", len(goals))

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return codex.Session{}, fmt.Errorf("выбор не прочитан (нет терминала?): укажите цель аргументом")
	}
	return Resolve(goals, line)
}
