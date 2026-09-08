// Command crescent restarts Codex goals as soon as usage limits reset.
//
// This iteration does one thing: it asks `codex app-server` what it knows, and
// prints the answer. If the limits and goals shown here match what the ChatGPT
// application shows, the whole foundation is proven — everything after this is
// a loop around three calls.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/unxed/crescent/internal/appserver"
	"github.com/unxed/crescent/internal/codexcli"
)

func main() {
	doctor := flag.Bool("doctor", false, "спросить app-server о лимитах и целях")
	codexPath := flag.String("codex", "", "путь к codex (иначе ищется автоматически)")
	verbose := flag.Bool("v", false, "показывать диагностику app-server")
	raw := flag.Bool("raw", false, "печатать сырые ответы сервера")
	flag.Parse()

	if !*doctor {
		fmt.Println("crescent — перезапуск целей Codex после сброса лимитов")
		fmt.Println()
		fmt.Println("  crescent -doctor    спросить app-server о лимитах и целях")
		fmt.Println()
		fmt.Println("Прежняя, файловая реализация сохранена: go run ./archive/cmd/crescent -run")
		return
	}
	if err := runDoctor(*codexPath, *verbose, *raw); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runDoctor(codexPath string, verbose, showRaw bool) error {
	if codexPath != "" {
		os.Setenv(codexcli.EnvCodex, codexPath)
	}
	path, tried := codexcli.FindCodex()
	if path == "" {
		fmt.Println("codex не найден. Искал здесь:")
		for _, t := range tried {
			fmt.Println("   ", t)
		}
		return fmt.Errorf("укажите путь: crescent -codex /путь/к/codex -doctor")
	}
	fmt.Println("codex:", path)

	// Said before anything is attempted: while the desktop application runs it
	// owns the session store, and a second owner is not possible.
	if running, name := codexcli.AppRunning(); running {
		fmt.Printf("\nВНИМАНИЕ: запущено приложение %s.\n", name)
		fmt.Println("Владелец сессий может быть только один, так что закройте его целиком.")
		fmt.Println("Пробую всё равно — посмотрим, что скажет сервер.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var stderr *os.File
	if verbose {
		stderr = os.Stderr
	}
	client, err := appserver.Dial(ctx, appserver.Options{
		CodexPath: path,
		Stderr:    stderr,
		OnNotification: func(method string, params json.RawMessage) {
			if verbose {
				fmt.Printf("  уведомление: %s\n", method)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("не удалось поговорить с app-server: %w", err)
	}
	defer client.Close()
	fmt.Println("app-server отвечает, рукопожатие прошло")

	fmt.Println("\nЛимиты аккаунта:")
	limits, rawLimits, err := client.RateLimits(ctx)
	if showRaw {
		fmt.Println("   сырой ответ:", short(string(rawLimits), 900))
	}
	if err != nil {
		fmt.Println("   не получены:", err)
	} else {
		windows := limits.Windows()
		if len(windows) == 0 {
			fmt.Println("   сервер не назвал ни одного окна")
		}
		for _, w := range windows {
			line := fmt.Sprintf("   %-11s израсходовано %.0f%%", w.Label(), w.UsedPercent)
			if at, ok := w.ResetAt(); ok {
				line += fmt.Sprintf(", сброс в %s (через %s)",
					at.Local().Format("15:04"), time.Until(at).Round(time.Minute))
			}
			fmt.Println(line)
		}
		if limits.OrdinaryUsageAllowed != nil {
			fmt.Printf("   сервер разрешает обычное использование: %v\n", *limits.OrdinaryUsageAllowed)
		}
		if limited, at := limits.Exhausted(); limited {
			if at.IsZero() {
				fmt.Println("   → сейчас работать нельзя, время сброса сервер не назвал")
			} else {
				fmt.Printf("   → сейчас работать нельзя, ждать до %s\n", at.Local().Format("15:04"))
			}
		} else {
			fmt.Println("   → работать можно")
		}
	}

	fmt.Println("\nЦели:")
	threads, rawThreads, err := client.Threads(ctx)
	if showRaw {
		fmt.Println("   сырой ответ:", short(string(rawThreads), 900))
	}
	if err != nil {
		return fmt.Errorf("thread/list: %w", err)
	}
	fmt.Printf("   тредов получено: %d\n", len(threads))

	shown := 0
	for _, t := range threads {
		if t.ID == "" {
			continue
		}
		goal, err := client.Goal(ctx, t.ID)
		if err != nil || !goal.Set() {
			continue
		}
		shown++
		fmt.Printf("\n   • %s\n", orDash(t.Label()))
		fmt.Printf("     %s  •  %s\n", orDash(goal.Status), orDash(t.Cwd))
		fmt.Printf("     %s\n", short(goal.Objective, 90))
	}
	if shown == 0 {
		fmt.Println("   ни у одного треда нет цели")
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func short(s string, n int) string {
	r := []rune(s)
	for i, c := range r {
		if c == '\n' || c == '\r' {
			r[i] = ' '
		}
	}
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}
