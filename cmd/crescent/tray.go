package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/unxed/crescent/internal/runner"
	"github.com/unxed/goWidgets"
	_ "github.com/unxed/goWidgets/backends/gtk"
	_ "github.com/unxed/goWidgets/backends/headless"
	_ "github.com/unxed/goWidgets/backends/win32"
)

// runTray is the shape the application was asked for: an icon in the status
// area, no console window, work happening quietly behind it.
//
// The daemon runs inside this process rather than beside it. Two processes
// would need a protocol between them and would let the icon outlive the work or
// the other way round; one process cannot get out of step with itself. The loop
// runs on a goroutine and touches the icon only through QueueUpdate.
type trayApp struct {
	app  *goWidgets.App
	dir  string
	pol  runner.Policy
	log  *os.File
	tray *goWidgets.TrayIcon

	pause  *goWidgets.MenuItem
	resume *goWidgets.MenuItem

	mu      sync.Mutex
	cancel  context.CancelFunc
	loop    *runner.Loop
	running bool
}

func runTray(dir string, readOnly, noProbe bool, prompt string) error {
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

	app, err := goWidgets.NewApp()
	if err != nil {
		return fmt.Errorf("не удалось открыть графическую подсистему: %w", err)
	}

	t := &trayApp{app: app, dir: dir, pol: pol}
	// With no console to print to, the log has to go somewhere findable — the
	// same file the terminal mode writes to, so there is one place to look.
	if f, err := runner.OpenLog(); err == nil {
		t.log = f
		defer f.Close()
	}

	t.pause = goWidgets.NewMenuItem("Пауза")
	t.resume = goWidgets.NewMenuItem("Продолжить")
	status := goWidgets.NewMenuItem("Состояние в журнал")
	quit := goWidgets.NewMenuItem("Выход")

	t.tray, err = app.NewTrayIcon("crescent — запускается",
		t.pause, t.resume, goWidgets.Separator(), status, goWidgets.Separator(), quit)
	if err != nil {
		return fmt.Errorf("трей недоступен: %w", err)
	}

	t.pause.Clicked.On(app.Scope(), func(struct{}) { t.stop("пауза") })
	t.resume.Clicked.On(app.Scope(), func(struct{}) { t.start(noProbe) })
	status.Clicked.On(app.Scope(), func(struct{}) { t.logStatus() })
	quit.Clicked.On(app.Scope(), func(struct{}) {
		t.stop("выход")
		app.Quit()
	})
	// Left click is the shortest path to "what is it doing?", and with no
	// window to open, writing the state to the log is that answer.
	t.tray.Activated.On(app.Scope(), func(struct{}) { t.logStatus() })

	t.start(noProbe)
	go t.pollStatus()

	return app.Run(nil)
}

func (t *trayApp) start(noProbe bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running {
		return
	}

	loop, err := runner.NewLoop(t.dir, t.pol)
	if err != nil {
		t.writef("не удалось запустить: %v", err)
		return
	}
	loop.ReadOnlyProbe = !noProbe
	if t.log != nil {
		loop.Log = t.log
	} else {
		loop.Log = os.Stdout
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.loop, t.cancel, t.running = loop, cancel, true
	t.writef("запуск")

	go func() {
		err := loop.Run(ctx)
		t.mu.Lock()
		t.running = false
		t.mu.Unlock()
		if err != nil {
			t.writef("остановлен: %v", err)
		}
	}()
}

func (t *trayApp) stop(why string) {
	t.mu.Lock()
	cancel, running := t.cancel, t.running
	t.mu.Unlock()
	if !running || cancel == nil {
		return
	}
	t.writef("%s", why)
	// Cancellation is noticed between turns: a half-finished Codex turn is
	// worse than a finished one, so a running turn is allowed to end.
	cancel()
}

// pollStatus keeps the tooltip current. It runs on its own goroutine and
// touches the icon only through QueueUpdate, as the toolkit requires.
func (t *trayApp) pollStatus() {
	for range time.Tick(2 * time.Second) {
		line := t.line()
		t.app.QueueUpdate(func() {
			if t.tray != nil {
				t.tray.Tooltip.Set(line)
			}
		})
	}
}

func (t *trayApp) line() string {
	t.mu.Lock()
	loop, running := t.loop, t.running
	t.mu.Unlock()

	if loop == nil {
		return "crescent — не запущен"
	}
	s := loop.Status()
	if !running {
		return "crescent — на паузе"
	}
	return "crescent — " + s.Line()
}

func (t *trayApp) logStatus() {
	t.mu.Lock()
	loop := t.loop
	t.mu.Unlock()
	if loop == nil {
		t.writef("не запущен")
		return
	}
	s := loop.Status()
	t.writef("%s | ходов %d, лимитов %d, ошибок %d", s.Line(), s.Turns, s.Limits, s.Errors)
}

func (t *trayApp) writef(format string, a ...any) {
	w := os.Stdout
	if t.log != nil {
		w = t.log
	}
	fmt.Fprintf(w, "%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}
