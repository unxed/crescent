// Command crescent watches Codex goal sessions and keeps them moving.
//
// This iteration does the reading half: it finds goal sessions on disk, shows
// them in a native window, and lets you tick off the ones you do not want. The
// part that actually resumes a session after a usage-limit reset comes next;
// nothing here starts, stops or writes to Codex.
//
//	crescent              native window (Win32 or GTK 3)
//	crescent -dump        text report — run this first, it is the fastest way
//	                      to find out whether the rollout parser understands
//	                      your Codex build
//	crescent -dump -keys  add the key names seen in the files (names only)
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/unxed/crescent/internal/codex"
	"github.com/unxed/goWidgets"
	_ "github.com/unxed/goWidgets/backends/gtk"
	_ "github.com/unxed/goWidgets/backends/headless"
	_ "github.com/unxed/goWidgets/backends/win32"
)

func main() {
	dump := flag.Bool("dump", false, "print what the scanner found and exit")
	keys := flag.Bool("keys", false, "with -dump: also list JSON key names seen (names only, no values)")
	flag.Parse()

	dir, err := codex.SessionsDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "не удалось определить каталог Codex:", err)
		os.Exit(1)
	}

	sessions, scanErr := codex.Scan(dir)
	if *dump {
		runDump(dir, sessions, scanErr, *keys)
		return
	}
	if scanErr != nil {
		fmt.Fprintf(os.Stderr, "не удалось прочитать %s: %v\n", dir, scanErr)
		fmt.Fprintln(os.Stderr, "запустите `crescent -dump` для диагностики")
		os.Exit(1)
	}
	if err := runGUI(dir, sessions); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runDump is the feedback loop for the one thing that cannot be verified
// anywhere but on a real machine: whether the rollout parser matches the format
// this particular Codex build writes.
func runDump(dir string, sessions []codex.Session, scanErr error, withKeys bool) {
	fmt.Println("crescent — отчёт сканера")
	fmt.Println("каталог сессий:", dir)
	if scanErr != nil {
		fmt.Println("ОШИБКА чтения:", scanErr)
		fmt.Println()
		fmt.Println("Если каталога нет — проверьте переменную CODEX_HOME или укажите путь")
		fmt.Println("вручную:  CODEX_HOME=/путь/к/.codex crescent -dump")
		return
	}

	var goals int
	for _, s := range sessions {
		if s.HasGoal() {
			goals++
		}
	}
	fmt.Printf("файлов: %d, из них с целью: %d\n\n", len(sessions), goals)

	// The limit belongs to the account, not to a session. Almost every rollout
	// carries a snapshot, but an old one only records the window that was
	// current when that session last ran — which is why a directory of old
	// sessions is full of reset times that lapsed days ago.
	fmt.Println("Лимиты аккаунта (по самому свежему rollout-файлу):")
	if from, windows, ok := codex.AccountLimits(sessions); ok {
		fmt.Printf("    источник: %s (изменён %s)\n",
			filepath.Base(from.Path), from.Modified.Format("2006-01-02 15:04:05"))
		for _, w := range windows {
			mark := "истекло"
			if d := time.Until(w.At); d > 0 {
				mark = "через " + d.Round(time.Minute).String()
			}
			fmt.Printf("    %-14s %s  (%s)   [поле: %s]\n",
				mark, w.At.Local().Format("2006-01-02 15:04:05"), humanWindow(w.At), w.Key)
		}
		if next := codex.NextReset(sessions); next.IsZero() {
			fmt.Println("    ни одно окно не активно — лимит сейчас не мешает")
		}
	} else {
		fmt.Println("    не найдено")
	}
	fmt.Println()

	shown := 0
	for _, s := range sessions {
		if !s.HasGoal() && shown >= 5 {
			continue // list a few goal-less files, then stop padding the report
		}
		shown++
		fmt.Printf("• %s\n", s.Title())
		fmt.Printf("    файл:     %s (%d КиБ, изменён %s)\n",
			s.Path, s.Size/1024, s.Modified.Format("2006-01-02 15:04:05"))
		fmt.Printf("    id:       %s  [из: %s]\n", orDash(s.ID), orDash(s.IDKey))
		fmt.Printf("    cwd:      %s\n", orDash(s.Cwd))
		if s.ParentID != "" {
			fmt.Printf("    продолж.: %s\n", s.ParentID)
		}
		fmt.Printf("    статус:   %s (возобновляемый: %v)\n", orDash(string(s.Status)), s.Status.Resumable())
		if s.ObjectiveKey != "" {
			fmt.Printf("    цель из:  поле %q\n", s.ObjectiveKey)
		}
		fmt.Printf("    окна:     %s\n", windowSummary(s))
	}

	if goals == 0 && len(sessions) > 0 {
		fmt.Println()
		fmt.Println("Целей не найдено ни в одном файле. Скорее всего парсер не знает")
		fmt.Println("имён полей вашей версии Codex. Перезапустите с -keys и пришлите")
		fmt.Println("вывод: там только ИМЕНА полей, без единого значения из ваших сессий.")
	}

	if withKeys {
		fmt.Println()
		fmt.Println("имена полей (частота), только имена:")
		total := map[string]int{}
		for i, s := range sessions {
			if i >= 20 {
				break
			}
			k, err := codex.Keys(s.Path)
			if err != nil {
				continue
			}
			for name, n := range k {
				total[name] += n
			}
		}
		names := make([]string, 0, len(total))
		for n := range total {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool { return total[names[i]] > total[names[j]] })
		for _, n := range names {
			fmt.Printf("    %-32s %d\n", n, total[n])
		}
	}
}

// humanWindow labels a window by how far out it sits: Codex runs a rolling
// five-hour allowance alongside a weekly one, and the distance tells them apart
// without having to know the key names.
func humanWindow(at time.Time) string {
	switch d := time.Until(at); {
	case d <= 0:
		return "уже прошло"
	case d <= 6*time.Hour:
		return "похоже на 5-часовое окно"
	default:
		return "похоже на недельное окно"
	}
}

func windowSummary(s codex.Session) string {
	if len(s.Resets) == 0 {
		return "не найдены"
	}
	parts := make([]string, 0, len(s.Resets))
	for _, r := range s.Resets {
		parts = append(parts, r.At.Local().Format("01-02 15:04"))
	}
	live := "все истекли"
	if n := s.ResetsAt(); !n.IsZero() {
		live = "ближайшее живое через " + time.Until(n).Round(time.Minute).String()
	}
	return fmt.Sprintf("%s — %s", strings.Join(parts, ", "), live)
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func runGUI(dir string, sessions []codex.Session) error {
	app, err := goWidgets.NewApp()
	if err != nil {
		return fmt.Errorf("не удалось открыть окно: %w", err)
	}

	win, err := app.NewWindow("crescent", 620, 420)
	if err != nil {
		return err
	}

	status, _ := win.AddLabel("")

	// Only goal sessions are actionable; the rest would be noise.
	var goals []codex.Session
	for _, s := range sessions {
		if s.HasGoal() && s.Status.Resumable() {
			goals = append(goals, s)
		}
	}
	if len(goals) > 12 {
		goals = goals[:12] // until there is a real ListView
	}

	boxes := make([]*goWidgets.CheckBox, 0, len(goals))
	for _, g := range goals {
		label := g.Title()
		box, err := win.AddCheckBox(label, true)
		if err != nil {
			return err
		}
		boxes = append(boxes, box)
	}
	if len(goals) == 0 {
		if _, err := win.AddLabel("Целей не найдено. Запустите crescent -dump."); err != nil {
			return err
		}
	}

	drop, _ := win.AddButton("Убрать снятые из очереди")
	quit, _ := win.AddButton("Выход")

	selected := func() int {
		n := 0
		for _, b := range boxes {
			if b.Checked.Get() && b.Visible.Get() {
				n++
			}
		}
		return n
	}
	next := codex.NextReset(sessions)
	refresh := func() {
		limit := "лимит свободен"
		if !next.IsZero() {
			limit = "лимит до " + next.Local().Format("15:04") +
				" (" + time.Until(next).Round(time.Minute).String() + ")"
		}
		status.Text.Set(fmt.Sprintf("Целей: %d, в очереди: %d. %s",
			len(goals), selected(), limit))
	}
	for _, b := range boxes {
		b.Toggled.On(app.Scope(), func(bool) { refresh() })
	}
	drop.Clicked.On(app.Scope(), func(goWidgets.ClickInfo) {
		for _, b := range boxes {
			if !b.Checked.Get() {
				b.Visible.Set(false)
			}
		}
		refresh()
	})
	quit.Clicked.On(app.Scope(), func(goWidgets.ClickInfo) { app.Quit() })
	win.Closing.On(app.Scope(), func(*goWidgets.CloseRequest) { app.Quit() })

	refresh()
	return app.Run(win)
}
