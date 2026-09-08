package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/unxed/crescent/internal/runner"
)

// runDaemon is the product: goals are pushed forward, one turn at a time, for
// as long as the process lives.
func runDaemon(dir string, readOnly, noProbe bool, prompt string) error {
	pol := runner.DefaultPolicy()
	if prompt != "" {
		pol.Prompt = prompt
	}
	if pol.CodexPath == "codex" {
		return fmt.Errorf("codex не найден; запустите crescent -doctor")
	}
	if readOnly {
		pol.Sandbox = "read-only"
		pol.WritableRoots = nil
	}

	loop, err := runner.NewLoop(dir, pol)
	if err != nil {
		return err
	}
	loop.ReadOnlyProbe = !noProbe

	fmt.Println("crescent — фоновый режим")
	fmt.Printf("  codex:     %s\n", pol.CodexPath)
	fmt.Printf("  песочница: %s\n", pol.Sandbox)
	if len(pol.WritableRoots) > 0 {
		fmt.Printf("  плюс запись в: %v\n", pol.WritableRoots)
	}
	fmt.Printf("  уступаю вам, если сессии менялись за последние %s\n", pol.QuietPeriod)
	fmt.Println("  Ctrl-C — остановить")
	fmt.Println()

	// Ctrl-C must stop the loop between turns, not kill a turn in progress —
	// a half-finished Codex turn is worse than a finished one.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return loop.Run(ctx)
}

// showStatus prints what a running daemon last published.
func showStatus() error {
	s, ok := runner.ReadStatus()
	if !ok {
		fmt.Println("Фоновый режим не запущен.")
		fmt.Println("Запустить:  crescent -run")
		return nil
	}

	fmt.Printf("Состояние: %s\n", s.Line())
	if s.Goal != "" {
		fmt.Printf("Цель:      %s\n", s.Goal)
	}
	if !s.Since.IsZero() {
		fmt.Printf("С:         %s (%s назад)\n",
			s.Since.Local().Format("15:04:05"), time.Since(s.Since).Round(time.Second))
	}
	if !s.StartedAt.IsZero() {
		fmt.Printf("Запущен:   %s\n", s.StartedAt.Local().Format("2006-01-02 15:04:05"))
	}
	fmt.Printf("Ходов: %d, упирался в лимит: %d, ошибок: %d\n", s.Turns, s.Limits, s.Errors)
	return nil
}
