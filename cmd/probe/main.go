// Command probe reproduces, once and in the open, exactly what crescent does to
// a goal — and reports everything it saw.
//
// It exists because the investigation was costing a round trip per question:
// run, collect, send, read, add one more recorder, run again. The probe answers
// the whole sequence in a single run: does the server accept a resume, does a
// turn start, does the model actually produce anything, does the turn reach an
// end, and what arrives that crescent normally discards.
//
// It drives one goal and stops. It does not watch, does not restart on a
// schedule, and does not decide anything is broken.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/unxed/crescent/internal/appserver"
	"github.com/unxed/crescent/internal/codexcli"
)

func main() {
	goalName := flag.String("goal", "", "цель по части имени (обязательно, если не -list)")
	list := flag.Bool("list", false, "показать цели и выйти")
	wait := flag.Duration("wait", 90*time.Second, "сколько ждать завершения хода")
	prompt := flag.String("prompt", "Продолжай работу над текущей целью.", "что послать цели")
	dry := flag.Bool("dry-run", false, "не запускать ход, только осмотреть")
	out := flag.String("out", "", "куда писать полный отчёт (по умолчанию рядом: probe-<время>.txt)")
	flag.Parse()

	if err := run(*goalName, *list, *dry, *prompt, *wait, *out); err != nil {
		fmt.Fprintln(os.Stderr, "ОШИБКА:", err)
		os.Exit(1)
	}
}

// tally counts what came back, by kind, so the report says what happened rather
// than making the reader page through a transcript.
type tally struct {
	mu       sync.Mutex
	methods  map[string]int
	deltas   int
	text     strings.Builder
	turnEnd  string
	turnID   string
	firstErr string
}

func (t *tally) note(method string, params json.RawMessage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.methods[method]++

	switch {
	case strings.HasSuffix(method, "/delta"):
		t.deltas++
		// The model's own words, reassembled. crescent drops these as noise,
		// which is why a working goal looked like silence.
		var d struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(params, &d) == nil {
			t.text.WriteString(d.Delta)
		}
	case method == "turn/completed" || method == "turn/failed" || method == "turn/aborted":
		t.turnEnd = method
	case method == "error":
		if t.firstErr == "" {
			t.firstErr = string(params)
		}
	}
}

func run(goalName string, list, dry bool, prompt string, wait time.Duration, outPath string) error {
	path, tried := codexcli.FindCodex()
	if path == "" {
		return fmt.Errorf("codex не найден; искал в %d местах", len(tried))
	}
	if outPath == "" {
		outPath = fmt.Sprintf("probe-%s.txt", time.Now().Format("15-04-05"))
	}
	report, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer report.Close()

	say := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		fmt.Println(line)
		fmt.Fprintln(report, line)
	}

	say("=== пробник crescent ===")
	say("время: %s", time.Now().Format(time.RFC3339))
	say("codex: %s", path)
	if running, name := codexcli.AppRunning(); running {
		say("ВНИМАНИЕ: запущено %s — владелец сессии может быть один", name)
	}

	t := &tally{methods: map[string]int{}}
	var wire strings.Builder

	ctx, cancel := context.WithTimeout(context.Background(), wait+2*time.Minute)
	defer cancel()

	client, err := appserver.Dial(ctx, appserver.Options{
		CodexPath: path,
		LogLevel:  "info",
		OnNotification: func(method string, params json.RawMessage) {
			t.note(method, params)
		},
	})
	if err != nil {
		return fmt.Errorf("не удалось поговорить с app-server: %w", err)
	}
	defer client.Close()

	var serverLines []string
	client.SetStderrSink(func(line string) {
		if strings.Contains(line, " ERROR ") || strings.Contains(line, " WARN ") {
			serverLines = append(serverLines, line)
		}
	})
	client.SetWireSink(func(dir, line string) {
		if len(line) > 400 {
			line = line[:400] + "…"
		}
		wire.WriteString(dir + " " + line + "\n")
	})
	say("рукопожатие прошло")

	// --- лимиты ---------------------------------------------------------
	limits, _, err := client.RateLimits(ctx)
	if err != nil {
		say("лимиты: НЕ ПРОЧИТАНЫ: %v", err)
	} else {
		for _, l := range limits.Summary() {
			say("лимит: %s", l)
		}
		if limited, at := limits.Exhausted(); limited {
			say("ЛИМИТ ИСЧЕРПАН, сброс в %s", at.Local().Format("15:04"))
		}
	}

	// --- цели -----------------------------------------------------------
	cands, err := client.Candidates(ctx)
	if err != nil {
		return fmt.Errorf("список целей: %w", err)
	}
	say("")
	say("--- цели (%d) ---", len(cands))
	for i, c := range cands {
		say("%2d. %-40s статус=%-12s токенов=%d", i+1,
			short(c.Thread.Label(), 40), c.Goal.Status, c.Goal.TokensUsed)
	}
	if list {
		say("")
		say("отчёт: %s", outPath)
		return nil
	}

	var chosen *appserver.Candidate
	for i := range cands {
		if goalName != "" && strings.Contains(strings.ToLower(cands[i].Thread.Label()), strings.ToLower(goalName)) {
			chosen = &cands[i]
			break
		}
	}
	if chosen == nil {
		return fmt.Errorf("цель по %q не найдена; посмотрите -list", goalName)
	}

	say("")
	say("--- выбрана: %s ---", chosen.Thread.Label())
	say("тред:   %s", chosen.Thread.ID)
	say("статус: %s", chosen.Goal.Status)
	say("токенов до запуска: %d, секунд: %d", chosen.Goal.TokensUsed, chosen.Goal.TimeUsedSeconds)
	if !appserver.Restartable(chosen.Goal.Status) {
		say("ВНИМАНИЕ: статус %q не перезапускается — crescent пропустил бы эту цель",
			chosen.Goal.Status)
	}
	if dry {
		say("(-dry-run: ход не запускается)")
		say("отчёт: %s", outPath)
		return nil
	}

	// --- запуск ---------------------------------------------------------
	say("")
	say("--- запускаю ход ---")
	started := time.Now()
	if err := client.Resume(ctx, chosen.Thread.ID); err != nil {
		say("thread/resume НЕ УДАЛСЯ: %v", err)
		return finish(say, t, serverLines, wire.String(), outPath)
	}
	say("thread/resume: принят")
	if err := client.StartTurn(ctx, chosen.Thread.ID, prompt); err != nil {
		say("turn/start НЕ УДАЛСЯ: %v", err)
		return finish(say, t, serverLines, wire.String(), outPath)
	}
	say("turn/start: принят")

	// --- ждём исхода ----------------------------------------------------
	say("жду до %s…", wait)
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		t.mu.Lock()
		end := t.turnEnd
		t.mu.Unlock()
		if end != "" {
			break
		}
	}

	// --- чем всё кончилось ----------------------------------------------
	after, gerr := client.Goal(ctx, chosen.Thread.ID)
	say("")
	say("--- итог за %s ---", time.Since(started).Round(time.Second))
	t.mu.Lock()
	if t.turnEnd != "" {
		say("ход завершился: %s", t.turnEnd)
	} else {
		say("ход НЕ завершился за отведённое время (это не обязательно ошибка)")
	}
	t.mu.Unlock()
	if gerr == nil {
		say("статус цели после: %s", after.Status)
		say("токенов после: %d (было %d, прирост %d)",
			after.TokensUsed, chosen.Goal.TokensUsed, after.TokensUsed-chosen.Goal.TokensUsed)
	} else {
		say("состояние цели после не прочитано: %v", gerr)
	}
	return finish(say, t, serverLines, wire.String(), outPath)
}

func finish(say func(string, ...any), t *tally, serverLines []string, wire, outPath string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	say("")
	say("--- что прислал сервер ---")
	keys := make([]string, 0, len(t.methods))
	for k := range t.methods {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		say("  %4d × %s", t.methods[k], k)
	}
	if len(keys) == 0 {
		say("  (ничего)")
	}

	say("")
	say("--- текст модели, собранный из дельт (%d фрагментов) ---", t.deltas)
	if text := strings.TrimSpace(t.text.String()); text != "" {
		say("%s", short(text, 3000))
	} else {
		say("(модель ничего не написала)")
	}

	if t.firstErr != "" {
		say("")
		say("--- первая ошибка от сервера ---")
		say("%s", short(t.firstErr, 500))
	}

	if len(serverLines) > 0 {
		say("")
		say("--- предупреждения сервера (%d, последние 10) ---", len(serverLines))
		from := len(serverLines) - 10
		if from < 0 {
			from = 0
		}
		for _, l := range serverLines[from:] {
			say("  %s", short(l, 220))
		}
	}

	// The transcript goes only into the file: it is long and it is for reading
	// afterwards, not for watching scroll past.
	if f, err := os.OpenFile(outPath, os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintln(f, "\n--- полный обмен ---")
		fmt.Fprintln(f, wire)
		f.Close()
	}
	fmt.Println("\nотчёт:", outPath)
	return nil
}

func short(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
