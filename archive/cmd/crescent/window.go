package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/unxed/crescent/archive/internal/codex"
	"github.com/unxed/crescent/archive/internal/runner"
	"github.com/unxed/goWidgets"
	_ "github.com/unxed/goWidgets/backends/gtk"
	_ "github.com/unxed/goWidgets/backends/headless"
	_ "github.com/unxed/goWidgets/backends/win32"
)

// desktopApp is crescent with a face: a window, an icon in the status area, and
// the daemon running behind both.
//
// One process holds all three. Splitting them would need a protocol between the
// parts and would let the icon outlive the work, or the window disagree with
// the icon about what is happening; a single process cannot get out of step
// with itself. The daemon runs on a goroutine and touches the interface only
// through QueueUpdate, which is what the toolkit requires.
type desktopApp struct {
	app *goWidgets.App
	dir string
	pol runner.Policy
	log *os.File

	win     *goWidgets.Window
	status  *goWidgets.Label
	counts  *goWidgets.Label
	runBtn  *goWidgets.Button
	tray    *goWidgets.TrayIcon
	hasTray bool

	goals []goalRow

	mu      sync.Mutex
	cancel  context.CancelFunc
	loop    *runner.Loop
	running bool
	noProbe bool
}

type goalRow struct {
	session codex.Session
	box     *goWidgets.CheckBox
}

// runDesktop opens the window and, when the desktop has a status area, the tray
// icon. visible decides whether the window starts on screen or tucked away.
func runDesktop(dir string, readOnly, noProbe bool, prompt string, visible bool) error {
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

	d := &desktopApp{app: app, dir: dir, pol: pol, noProbe: noProbe}
	if f, err := runner.OpenLog(); err == nil {
		d.log = f
		defer f.Close()
	}

	sessions, err := codex.Scan(dir)
	if err != nil {
		return fmt.Errorf("каталог сессий не прочитан: %w", err)
	}

	if err := d.build(sessions); err != nil {
		return err
	}
	d.start()
	go d.poll()

	if !visible {
		d.win.Hide()
	}
	return app.Run(d.win)
}

func (d *desktopApp) build(sessions []codex.Session) error {
	win, err := d.app.NewWindow("crescent", 560, 420)
	if err != nil {
		return err
	}
	d.win = win

	d.status, _ = win.AddLabel("")
	d.counts, _ = win.AddLabel("")

	// Goals are listed as check boxes: unchecking one takes it out of the queue
	// without stopping anything else. Ten is as many as a plain vertical stack
	// can show honestly; a real list widget comes later.
	goals := runner.Goals(sessions)
	if len(goals) > 10 {
		goals = goals[:10]
	}
	for _, g := range goals {
		label := g.Label()
		if !workspaceLive(g) {
			label = "✗ " + label
		}
		box, err := win.AddCheckBox(label, workspaceLive(g))
		if err != nil {
			return err
		}
		row := goalRow{session: g, box: box}
		d.goals = append(d.goals, row)
		box.Toggled.On(d.app.Scope(), func(bool) { d.applySelection() })
	}
	if len(goals) == 0 {
		if _, err := win.AddLabel("Целей не найдено. Проверьте crescent -doctor."); err != nil {
			return err
		}
	}

	d.runBtn, _ = win.AddButton("Пауза")
	d.runBtn.Clicked.On(d.app.Scope(), func(goWidgets.ClickInfo) { d.toggle() })

	hide, _ := win.AddButton("Свернуть")
	hide.Clicked.On(d.app.Scope(), func(goWidgets.ClickInfo) { d.win.Hide() })

	quit, _ := win.AddButton("Выход")
	quit.Clicked.On(d.app.Scope(), func(goWidgets.ClickInfo) { d.quit() })

	d.buildTray()

	// Closing hides to the tray when there is one, and quits when there is not:
	// hiding a window with no way back is just losing it.
	win.Closing.On(d.app.Scope(), func(req *goWidgets.CloseRequest) {
		if d.hasTray {
			req.CancelClose()
			d.win.Hide()
			return
		}
		d.quit()
	})

	d.applySelection()
	d.refresh()
	return nil
}

func (d *desktopApp) buildTray() {
	show := goWidgets.NewMenuItem("Показать окно")
	pause := goWidgets.NewMenuItem("Пауза / продолжить")
	quit := goWidgets.NewMenuItem("Выход")

	tray, err := d.app.NewTrayIcon("crescent", show, pause, goWidgets.Separator(), quit)
	if err != nil {
		d.writef("трей недоступен (%v) — работаем окном", err)
		return
	}
	d.tray, d.hasTray = tray, true

	show.Clicked.On(d.app.Scope(), func(struct{}) { d.win.Show() })
	pause.Clicked.On(d.app.Scope(), func(struct{}) { d.toggle() })
	quit.Clicked.On(d.app.Scope(), func(struct{}) { d.quit() })
	tray.Activated.On(d.app.Scope(), func(struct{}) { d.win.Show() })
}

// applySelection hands the current check-box state to the running loop, so
// unchecking a goal takes effect on the next turn rather than on restart.
func (d *desktopApp) applySelection() {
	disabled := map[string]bool{}
	for _, g := range d.goals {
		if !g.box.Checked.Get() {
			disabled[g.session.ID] = true
		}
	}
	d.mu.Lock()
	d.pol.Disabled = disabled
	loop := d.loop
	d.mu.Unlock()

	if loop != nil {
		loop.SetDisabled(disabled)
	}
	d.refresh()
}

func (d *desktopApp) start() {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return
	}
	loop, err := runner.NewLoop(d.dir, d.pol)
	if err != nil {
		d.mu.Unlock()
		d.writef("не удалось запустить: %v", err)
		return
	}
	loop.ReadOnlyProbe = !d.noProbe
	if d.log != nil {
		loop.Log = d.log
	} else {
		loop.Log = os.Stdout
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.loop, d.cancel, d.running = loop, cancel, true
	d.mu.Unlock()

	d.writef("запуск")
	go func() {
		err := loop.Run(ctx)
		d.mu.Lock()
		d.running = false
		d.mu.Unlock()
		if err != nil {
			d.writef("остановлен: %v", err)
		}
	}()
}

func (d *desktopApp) stop() {
	d.mu.Lock()
	cancel, running := d.cancel, d.running
	d.mu.Unlock()
	if !running || cancel == nil {
		return
	}
	d.writef("пауза")
	// Noticed between turns: a half-finished Codex turn is worse than a
	// finished one, so a turn already under way is allowed to end.
	cancel()
}

func (d *desktopApp) toggle() {
	d.mu.Lock()
	running := d.running
	d.mu.Unlock()
	if running {
		d.stop()
	} else {
		d.start()
	}
	d.refresh()
}

func (d *desktopApp) quit() {
	d.stop()
	d.app.Quit()
}

// poll keeps the window and the tooltip current. It runs on its own goroutine
// and touches widgets only through QueueUpdate.
func (d *desktopApp) poll() {
	for range time.Tick(time.Second) {
		d.app.QueueUpdate(d.refresh)
	}
}

func (d *desktopApp) refresh() {
	d.mu.Lock()
	loop, running := d.loop, d.running
	d.mu.Unlock()

	line := "не запущен"
	turns, limits, errs := 0, 0, 0
	if loop != nil {
		s := loop.Status()
		turns, limits, errs = s.Turns, s.Limits, s.Errors
		if running {
			line = s.Line()
		} else {
			line = "на паузе"
		}
	}

	queued := 0
	for _, g := range d.goals {
		if g.box.Checked.Get() {
			queued++
		}
	}

	d.status.Text.Set(line)
	d.counts.Text.Set(fmt.Sprintf("В очереди: %d из %d  •  ходов: %d, лимитов: %d, ошибок: %d",
		queued, len(d.goals), turns, limits, errs))
	if running {
		d.runBtn.Text.Set("Пауза")
	} else {
		d.runBtn.Text.Set("Гнать все цели")
	}
	if d.hasTray {
		d.tray.Tooltip.Set("crescent — " + line)
	}
}

func (d *desktopApp) writef(format string, a ...any) {
	w := os.Stdout
	if d.log != nil {
		w = d.log
	}
	fmt.Fprintf(w, "%s  %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}
