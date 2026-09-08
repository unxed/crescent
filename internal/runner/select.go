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
		fmt.Fprintf(&b, "    %s  %s\n", ShortID(m.ID), m.Label())
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

// WriteList prints the numbered chooser. runnable reports whether a session can
// actually be continued — many working directories were created under /tmp and
// did not survive a reboot, and picking one of those only wastes a turn.
func WriteList(w io.Writer, goals []codex.Session, runnable func(codex.Session) bool) {
	if len(goals) == 0 {
		fmt.Fprintln(w, "Целей не найдено.")
		return
	}
	marked := false
	for i, g := range goals {
		mark := "  "
		if runnable != nil && !runnable(g) {
			mark = "✗ " // workspace gone
			marked = true
		}
		fmt.Fprintf(w, "%s%2d. %-9s %-9s %s\n", mark, i+1, ShortID(g.ID), g.Status, g.Label())
	}
	// The legend is printed only when something actually carries the mark;
	// explaining a symbol that does not appear is noise.
	if marked {
		fmt.Fprintln(w, "\n✗ — рабочий каталог не существует, продолжать нечего")
	}
}

// Default is the goal to offer without being asked: the freshest one that can
// actually be continued. After you stop a goal in the app to work on it, that
// goal is the most recently written, so this is nearly always the intended one.
func Default(goals []codex.Session, runnable func(codex.Session) bool) (codex.Session, bool) {
	for _, g := range goals {
		if !g.Status.Resumable() {
			continue
		}
		if runnable != nil && !runnable(g) {
			continue
		}
		return g, true
	}
	return codex.Session{}, false
}

// Pick shows the list with the default already chosen, so the whole interaction
// is one keystroke. Nothing has to be memorised and nothing has to be typed:
// Enter accepts, a number overrides, q cancels.
func Pick(in io.Reader, out io.Writer, goals []codex.Session, runnable func(codex.Session) bool) (codex.Session, error) {
	def, hasDef := Default(goals, runnable)

	WriteList(out, goals, runnable)
	if !hasDef {
		return codex.Session{}, fmt.Errorf("нет ни одной цели, которую можно продолжить")
	}

	defNum := 0
	for i, g := range goals {
		if g.ID == def.ID {
			defNum = i + 1
		}
	}
	fmt.Fprintf(out, "\nПродолжить цель %d (%s)? [Enter — да, номер — другая, q — отмена]: ",
		defNum, oneLine(def.Label(), 46))

	line, err := bufio.NewReader(in).ReadString('\n')
	answer := strings.TrimSpace(line)
	if err != nil && answer == "" {
		return codex.Session{}, fmt.Errorf("выбор не прочитан (нет терминала?): укажите цель аргументом или добавьте -yes")
	}
	switch strings.ToLower(answer) {
	case "":
		return def, nil
	case "q", "n", "нет", "отмена":
		return codex.Session{}, ErrNoSelection{}
	}
	return Resolve(goals, answer)
}
