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
	"strings"
	"time"

	"github.com/unxed/crescent/internal/appserver"
	"github.com/unxed/crescent/internal/codexcli"
)

func main() {
	doctor := flag.Bool("doctor", false, "спросить app-server о лимитах и целях")
	codexPath := flag.String("codex", "", "путь к codex (иначе ищется автоматически)")
	verbose := flag.Bool("v", false, "показывать диагностику app-server")
	raw := flag.Bool("raw", false, "печатать сырые ответы сервера")
	restart := flag.Bool("restart", false, "перезапустить остановленные цели, если лимит позволяет")
	gui := flag.Bool("gui", false, "окно: отметить цели галочками и вести их в фоне")
	watch := flag.String("watch", "", "вести названные цели в терминале (через запятую; пусто — все закреплённые)")
	dry := flag.Bool("dry-run", false, "с -restart: показать, что было бы сделано, и не делать")
	prompt := flag.String("prompt", "Продолжай работу над текущей целью.", "чем будить цель")
	flag.Parse()

	if *gui {
		if err := runDesktop(*codexPath, *prompt, *verbose); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if *watch != "" || hasFlag("watch") {
		sels := splitList(*watch)
		if err := runWatch(*codexPath, sels, *prompt, *verbose, 30*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if *restart {
		if err := runRestart(*codexPath, *dry, *prompt, *verbose); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if !*doctor {
		fmt.Println("crescent — перезапуск целей Codex после сброса лимитов")
		fmt.Println()
		fmt.Println("  crescent -doctor              спросить app-server о лимитах и целях")
		fmt.Println("  crescent -restart -dry-run    показать, что было бы перезапущено")
		fmt.Println("  crescent -restart             перезапустить остановленные цели")
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
	// Reading turns out not to conflict: the thread list arrives whether or not
	// the application is running. Only taking a turn needs sole ownership, so
	// the warning belongs there rather than here.
	if running, name := codexcli.AppRunning(); running {
		fmt.Printf("(запущено приложение %s — на чтение не влияет)\n", name)
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
	if len(threads) == 0 {
		// Printed without being asked: an empty list with no error means the
		// answer was read wrongly, and the answer itself is the only thing
		// that settles it.
		fmt.Println("   пусто — вот что прислал сервер:")
		fmt.Println("  ", short(string(rawThreads), 1200))
	}

	shown, failed := 0, 0
	for _, t := range threads {
		if t.ID == "" {
			continue
		}
		goal, err := client.Goal(ctx, t.ID)
		if err != nil {
			// Worth naming: a thread that has to be loaded before its goal can
			// be read is a different problem from a thread without a goal.
			if failed == 0 {
				fmt.Printf("   thread/goal/get не отработал (%s): %v\n", short(t.Label(), 40), err)
			}
			failed++
			continue
		}
		if !goal.Set() {
			continue
		}
		shown++
		fmt.Printf("\n   • %s\n", orDash(t.Label()))
		fmt.Printf("     цель: %s  •  %s\n", orDash(goal.Status), orDash(t.Cwd))
		fmt.Printf("     %s\n", short(goal.Objective, 90))
	}
	if failed > 0 {
		fmt.Printf("\n   целей не удалось прочитать: %d\n", failed)
	}
	if shown == 0 {
		fmt.Println("\n   ни у одного треда нет цели")
	}
	return nil
}

func hasFlag(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
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

// runRestart is the product in its smallest honest form: if the account may
// work, push every stopped goal forward by one turn.
func runRestart(codexPath string, dry bool, prompt string, verbose bool) error {
	client, path, err := connect(codexPath, verbose)
	if err != nil {
		return err
	}
	defer client.Close()
	fmt.Println("codex:", path)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	limits, err := client.RateLimitsWithRetry(ctx, 3)
	if err != nil {
		// Unknown is not permission. Refusing to act on an unread limit is the
		// difference between a careful tool and one that hammers a closed door.
		return fmt.Errorf("состояние лимитов не выяснено, ничего не запускаю: %w", err)
	}

	limited, at := limits.Exhausted()
	if limited {
		// Said plainly, because the limit belongs to the account and not to any
		// one goal: a bare "limit reached" reads as if some particular chat had
		// run out, which would be a different and much smaller problem.
		fmt.Print("Лимит аккаунта исчерпан — он общий на все цели сразу")
		if at.IsZero() {
			fmt.Println(", время сброса сервер не назвал.")
		} else {
			fmt.Printf(", сброс в %s (через %s).\n",
				at.Local().Format("15:04"), time.Until(at).Round(time.Minute))
		}
	} else {
		fmt.Println("Лимит аккаунта позволяет работать.")
	}

	// The queue is shown either way. A dry run that stops at the limit answers
	// the wrong question: what is asked is which goals would be pushed, and
	// that does not depend on whether the window happens to be open now.
	candidates, err := client.Candidates(ctx)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		fmt.Println("Остановленных целей нет.")
		return nil
	}

	if limited {
		fmt.Printf("\nЖдут сброса (%d):\n", len(candidates))
		for _, c := range candidates {
			fmt.Printf(" • %-34s [%s]  %s\n",
				short(orDash(c.Thread.Label()), 34), orDash(c.Goal.Status),
				short(c.Goal.Objective, 60))
		}
		fmt.Println("\nПосле сброса эти цели можно будет запустить тем же crescent -restart.")
		return nil
	}

	// Only taking a turn needs sole ownership of a thread; reading did not.
	if running, name := codexcli.AppRunning(); running && !dry {
		return fmt.Errorf("запущено приложение %s — закройте его: владелец сессии может быть только один", name)
	}

	fmt.Printf("\nЦелей к перезапуску: %d\n", len(candidates))
	for _, c := range candidates {
		fmt.Printf("\n • %s  [%s]\n   %s\n",
			orDash(c.Thread.Label()), orDash(c.Goal.Status), short(c.Goal.Objective, 80))
		if dry {
			continue
		}
		if err := client.Restart(ctx, c.Thread.ID, prompt); err != nil {
			fmt.Printf("   не удалось: %v\n", err)
			continue
		}
		fmt.Println("   запущено")
	}
	if dry {
		fmt.Println("\n(-dry-run: ничего не запускалось)")
	}
	return nil
}

// connect starts an app-server and completes the handshake.
func connect(codexPath string, verbose bool) (*appserver.Client, string, error) {
	if codexPath != "" {
		os.Setenv(codexcli.EnvCodex, codexPath)
	}
	path, tried := codexcli.FindCodex()
	if path == "" {
		return nil, "", fmt.Errorf("codex не найден; искал в %d местах, укажите путь через -codex", len(tried))
	}
	var stderr *os.File
	if verbose {
		stderr = os.Stderr
	}
	client, err := appserver.Dial(context.Background(), appserver.Options{
		CodexPath: path,
		Stderr:    stderr,
	})
	if err != nil {
		return nil, path, fmt.Errorf("не удалось поговорить с app-server: %w", err)
	}
	return client, path, nil
}
