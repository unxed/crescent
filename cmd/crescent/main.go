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

	"context"

	"github.com/unxed/crescent/internal/codex"
	"github.com/unxed/crescent/internal/runner"
	"github.com/unxed/goWidgets"
	_ "github.com/unxed/goWidgets/backends/gtk"
	_ "github.com/unxed/goWidgets/backends/headless"
	_ "github.com/unxed/goWidgets/backends/win32"
)

func main() {
	dump := flag.Bool("dump", false, "print what the scanner found and exit")
	keys := flag.Bool("keys", false, "with -dump: also list JSON key names seen (names only, no values)")
	doctor := flag.Bool("doctor", false, "check the machine: codex CLI, its flags, Go caches, workspaces")
	plan := flag.Bool("plan", false, "print what crescent would run, without running anything")
	codexPath := flag.String("codex", "", "path to the codex binary (overrides the search and "+runner.EnvCodex+")")
	list := flag.Bool("list", false, "print the numbered list of goals and exit")
	runOnce := flag.Bool("run-once", false, "resume exactly one turn, then stop; takes a number, id prefix or goal text")
	readOnly := flag.Bool("read-only", false, "with -run-once: force --sandbox read-only (recommended for the first run)")
	rawTo := flag.String("raw", "", "with -run-once: write the raw --json stream to this file")
	prompt := flag.String("prompt", "", "message used to wake the session")
	yes := flag.Bool("yes", false, "with -run-once: take the offered goal without asking")
	gui := flag.Bool("gui", false, "open the window (it is still a viewer: nothing can be started from it)")
	find := flag.String("find", "", "find which JSON key holds this text (e.g. a chat name you can see in the app)")
	deep := flag.Bool("deep", false, "with -find: search the whole Codex directory, not only sessions")
	refresh := flag.Bool("refresh", false, "forget the scan cache and read every rollout again")
	flag.Parse()

	// Go's flag package stops parsing at the first positional argument, so
	// `-run-once 1 -read-only` would silently drop -read-only and run with a
	// writable sandbox — the exact opposite of what was asked for. Keep
	// consuming: positionals become the selector, flags are parsed wherever
	// they appear.
	selector := ""
	for rest := flag.Args(); len(rest) > 0; rest = flag.Args() {
		if strings.HasPrefix(rest[0], "-") {
			if err := flag.CommandLine.Parse(rest); err != nil {
				os.Exit(2)
			}
			continue
		}
		if selector == "" {
			selector = rest[0]
		}
		if err := flag.CommandLine.Parse(rest[1:]); err != nil {
			os.Exit(2)
		}
	}

	if *codexPath != "" {
		os.Setenv(runner.EnvCodex, *codexPath)
	}

	dir, err := codex.SessionsDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "не удалось определить каталог Codex:", err)
		os.Exit(1)
	}

	if *doctor {
		runDoctor(dir)
		return
	}

	sessions, scanErr := codex.ScanWithProgress(dir, scanProgress())
	finishProgress()

	if *refresh {
		if err := codex.DropCache(); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "не удалось сбросить кэш:", err)
		} else {
			fmt.Println("Кэш сброшен, следующий запуск прочитает все файлы заново.")
		}
	}

	if *find != "" {
		root := dir
		if *deep {
			if h, err := codex.Home(); err == nil {
				root = h
			}
		}
		if err := runFind(root, *find); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if *gui {
		if scanErr != nil {
			fmt.Fprintln(os.Stderr, "не удалось прочитать", dir+":", scanErr)
			os.Exit(1)
		}
		if err := runGUI(dir, sessions); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if *list || *runOnce {
		if scanErr != nil {
			fmt.Fprintln(os.Stderr, "не удалось прочитать", dir+":", scanErr)
			os.Exit(1)
		}
		goals := runner.Goals(sessions)
		if *list {
			runner.WriteList(os.Stdout, goals, workspaceLive)
			return
		}
		if err := doRunOnce(goals, selector, *readOnly, *yes, *rawTo, *prompt); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if *plan {
		if scanErr != nil {
			fmt.Fprintln(os.Stderr, "не удалось прочитать", dir+":", scanErr)
			os.Exit(1)
		}
		runPlan(sessions)
		return
	}
	if *dump {
		runDump(dir, sessions, scanErr, *keys)
		return
	}
	// Bare invocation used to open the window, which only shows what was found
	// and cannot start anything — leaving the obvious next step invisible.
	if scanErr != nil {
		fmt.Fprintf(os.Stderr, "не удалось прочитать %s: %v\n", dir, scanErr)
		fmt.Fprintln(os.Stderr, "запустите `crescent -doctor` для диагностики")
		os.Exit(1)
	}
	goals := runner.Goals(sessions)
	runner.WriteList(os.Stdout, goals, workspaceLive)
	fmt.Println()
	if _, ok := runner.Default(goals, workspaceLive); ok {
		fmt.Println("Продолжить цель на один ход:")
		fmt.Println("    crescent -run-once -read-only      предложит подходящую, Enter соглашается")
		fmt.Println("    crescent -run-once -read-only 2    сразу нужную")
	}
	fmt.Println("Ещё:  -plan (очередь)   -doctor (окружение)   -dump (подробно)   -gui (окно)")
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
	fmt.Println("Лимиты аккаунта (снимок из самого свежего rollout-файла):")
	if from, windows, ok := codex.AccountLimits(sessions); ok {
		live := 0
		for _, w := range windows {
			if time.Until(w.At) <= 0 {
				continue
			}
			live++
			fmt.Printf("    %-9s до %s   (%s)\n",
				"через "+time.Until(w.At).Round(time.Minute).String(),
				w.At.Local().Format("2006-01-02 15:04"), humanWindow(w.At))
		}
		if live == 0 {
			fmt.Println("    все окна уже прошли — лимит сейчас не мешает")
		}
		fmt.Printf("    снимок от %s\n", from.Modified.Format("2006-01-02 15:04:05"))
	} else {
		fmt.Println("    не найдено")
	}
	fmt.Println()

	fmt.Println("Цели:")
	skipped := 0
	for _, s := range sessions {
		// Sessions without a goal are the overwhelming majority and there is
		// nothing to resume in them; counting them is enough.
		if !s.HasGoal() {
			skipped++
			continue
		}
		fmt.Printf("\n• %s\n", s.Label())
		fmt.Printf("    %s  •  %s  •  изменён %s\n",
			orDash(string(s.Status)), orDash(s.Cwd), s.Modified.Format("2006-01-02 15:04"))
		fmt.Printf("    id %s", orDash(s.ID))
		if s.ParentID != "" {
			fmt.Printf(", продолжает %s", s.ParentID)
		}
		fmt.Printf("  [%s]\n", orDash(s.IDKey))
		if s.ObjectiveKey != "" || s.NameKey != "" {
			fmt.Printf("    цель из поля %q, имя чата из %q\n",
				orDash(s.ObjectiveKey), orDash(s.NameKey))
		}
		fmt.Printf("    %s\n", filepath.Base(s.Path))
	}
	fmt.Printf("\nБез цели пропущено: %d\n", skipped)

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

// runDoctor answers "will this actually work here?" before a single Codex turn
// is spent on finding out.
func runDoctor(dir string) {
	fmt.Println("crescent — проверка окружения")
	fmt.Println()

	d := runner.Diagnose()
	fmt.Println("Codex CLI:")
	if d.CodexPath == "" {
		fmt.Println("    не найден. Искал здесь:")
		for _, t := range d.Tried {
			fmt.Println("        " + t)
		}
		fmt.Println("    Если codex стоит в другом месте, покажите путь:")
		fmt.Println("        crescent -codex /путь/к/codex -doctor")
		fmt.Println("    Узнать путь можно так:  type -a codex   или   command -v codex")
	} else {
		fmt.Printf("    %s\n    версия: %s\n", d.CodexPath, orDash(d.CodexVersion))
		fmt.Printf("    exec: %v, exec resume: %v, --json: %v\n", d.HasExec, d.HasResume, d.HasJSON)
	}

	fmt.Println()
	fmt.Println("Каталоги, которым нужен доступ на запись вне песочницы:")
	for _, r := range runner.GoCaches() {
		mark := "нет"
		if fi, err := os.Stat(r); err == nil && fi.IsDir() {
			mark = "есть"
		}
		fmt.Printf("    %-40s %s\n", r, mark)
	}

	fmt.Println()
	fmt.Println("Рабочие каталоги целей:")
	sessions, err := codex.Scan(dir)
	if err != nil {
		fmt.Println("    каталог сессий не прочитан:", err)
	} else {
		live, gone := 0, 0
		for _, s := range sessions {
			if !s.HasGoal() {
				continue
			}
			mark := "НЕТ "
			if fi, err := os.Stat(s.Cwd); err == nil && fi.IsDir() {
				mark = "есть"
				live++
			} else {
				gone++
			}
			fmt.Printf("    %s  %-46s  %s\n", mark, truncate(orDash(s.Cwd), 46), s.Label())
		}
		fmt.Printf("    итого: %d живых, %d исчезло\n", live, gone)
	}

	if len(d.Problems) > 0 {
		fmt.Println()
		fmt.Println("Проблемы:")
		for _, p := range d.Problems {
			fmt.Println("    •", p)
		}
	}
}

// runPlan prints the decision without acting on it. Everything crescent would
// do unattended is visible here first.
func runPlan(sessions []codex.Session) {
	pol := runner.DefaultPolicy()
	pl := runner.Build(sessions, pol, time.Now())

	fmt.Println("crescent — план (ничего не запускается)")
	fmt.Println()
	if pol.CodexPath == "codex" {
		fmt.Println("ВНИМАНИЕ: codex не найден, команды напечатаны с голым именем.")
		fmt.Println("Запустите crescent -doctor, чтобы увидеть, где он искался.")
		fmt.Println()
	}

	switch {
	case pl.Interrupt:
		fmt.Printf("СТОП: в каталоге сессий только что была запись (тише %s назад).\n",
			pol.QuietPeriod)
		fmt.Println("Похоже, вы работаете в Codex сами — crescent в это не вмешивается.")
	case pl.Limited:
		fmt.Printf("Лимит закрыт до %s (через %s) — очередь ждёт.\n",
			pl.StartAt.Local().Format("2006-01-02 15:04"),
			time.Until(pl.StartAt).Round(time.Minute))
	default:
		fmt.Println("Лимит открыт, очередь может идти сейчас.")
	}

	fmt.Printf("\nОчередь (последовательно, по одной цели за раз):\n")
	n := 0
	for _, st := range pl.Steps {
		if st.Skip != "" {
			continue
		}
		n++
		fmt.Printf("\n%d. %s\n", n, st.Session.Label())
		fmt.Printf("   %s\n", shellLine(pol.CodexPath, st.Args))
	}
	if n == 0 {
		fmt.Println("   пусто")
	}

	fmt.Printf("\nПропущено:\n")
	skipped := 0
	for _, st := range pl.Steps {
		if st.Skip == "" {
			continue
		}
		skipped++
		fmt.Printf("   %s — %s\n", st.Session.Label(), st.Skip)
	}
	if skipped == 0 {
		fmt.Println("   ничего")
	}
}

// doRunOnce resumes one named session for a single turn and reports what
// happened. It is deliberately a separate mode from the queue: before anything
// runs unattended, one turn should be watched by a human.
func doRunOnce(goals []codex.Session, selector string, readOnly, yes bool, rawPath, prompt string) error {
	if len(goals) == 0 {
		return fmt.Errorf("целей не найдено; проверьте crescent -dump")
	}

	// No selector means "show me what there is": typing a 36-character uuid by
	// hand is not a user interface.
	var target codex.Session
	var err error
	switch {
	case strings.TrimSpace(selector) != "":
		target, err = runner.Resolve(goals, selector)
	case yes:
		var ok bool
		target, ok = runner.Default(goals, workspaceLive)
		if !ok {
			err = fmt.Errorf("нет ни одной цели, которую можно продолжить")
		}
	default:
		target, err = runner.Pick(os.Stdin, os.Stdout, goals, workspaceLive)
	}
	if err != nil {
		return err
	}
	fmt.Println()

	pol := runner.DefaultPolicy()
	if prompt != "" {
		pol.Prompt = prompt
	}
	if pol.CodexPath == "codex" {
		return fmt.Errorf("codex не найден; запустите crescent -doctor")
	}

	opts := runner.Options{ReadOnly: readOnly, Trace: os.Stdout, Timeout: 30 * time.Minute}
	if rawPath != "" {
		f, err := os.Create(rawPath)
		if err != nil {
			return err
		}
		defer f.Close()
		opts.RawTo = f
	}

	if !workspaceLive(target) {
		return fmt.Errorf("у цели %q рабочий каталог %s не существует — продолжать нечего",
			target.Label(), target.Cwd)
	}

	fmt.Printf("Цель:    %s\n", target.Label())
	fmt.Printf("id:      %s\n", target.ID)
	fmt.Printf("Каталог: %s\n", target.Cwd)
	if readOnly {
		fmt.Println("Песочница: read-only — запись невозможна, это проверка связки.")
	} else {
		fmt.Printf("Песочница: %s + кэши Go\n", pol.Sandbox)
	}
	fmt.Println("\nСобытия:")

	res, err := runner.RunOnce(context.Background(), pol, target, opts)
	if err != nil {
		return err
	}

	fmt.Printf("\nИтог за %s: код выхода %d, строк %d (разобрано %d)\n",
		res.Duration.Round(time.Second), res.ExitCode, res.Lines, res.Parsed)
	if len(res.EventTypes) > 0 {
		fmt.Println("Типы событий:")
		for k, n := range res.EventTypes {
			fmt.Printf("    %-32s %d\n", k, n)
		}
	}
	if res.UsageLimited {
		fmt.Println("УПЁРЛИСЬ В ЛИМИТ — ровно тот случай, ради которого всё затевалось.")
		if !res.ResetsAt.IsZero() {
			fmt.Printf("    сброс в %s (через %s)\n",
				res.ResetsAt.Local().Format("2006-01-02 15:04"),
				time.Until(res.ResetsAt).Round(time.Minute))
		}
	}
	for _, e := range res.Errors {
		fmt.Println("Ошибка:", e)
	}
	if res.LastMessage != "" {
		fmt.Printf("\nПоследнее сообщение:\n    %s\n", truncate(res.LastMessage, 400))
	}
	if res.Parsed == 0 && res.Lines > 0 {
		fmt.Println("\nНи одна строка не разобралась как JSON. Сохраните поток")
		fmt.Println("через -raw и пришлите — формат событий поменялся.")
	}
	return nil
}

// workspaceLive reports whether a session's working directory still exists.
// Many sessions ran out of /tmp and did not survive a reboot.
func workspaceLive(s codex.Session) bool {
	fi, err := os.Stat(s.Cwd)
	return err == nil && fi.IsDir()
}

// shellLine renders argv so it can be pasted into a terminal as-is. The prompt
// contains spaces, so an unquoted join would print a command that means
// something different from the one crescent would run.
func shellLine(bin string, args []string) string {
	if bin == "" {
		bin = "codex"
	}
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, maybeQuote(bin))
	for _, a := range args {
		quoted = append(quoted, maybeQuote(a))
	}
	return strings.Join(quoted, " ")
}

func maybeQuote(a string) string {
	if a == "" || strings.ContainsAny(a, " \t\n\"'\\$`*?[]{}()<>|&;#~") {
		return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return a
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
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
		label := g.Label()
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
