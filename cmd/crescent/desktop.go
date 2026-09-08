package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"time"

	gw "github.com/unxed/goWidgets"
	_ "github.com/unxed/goWidgets/backends/gtk"
	_ "github.com/unxed/goWidgets/backends/headless"
	_ "github.com/unxed/goWidgets/backends/win32"

	"github.com/unxed/crescent/internal/appserver"
	"github.com/unxed/crescent/internal/journal"
	"github.com/unxed/crescent/internal/pins"
	"github.com/unxed/crescent/internal/supervisor"
)

// desktop is crescent with a face. The design target is a tired person at four
// in the morning: one glance at the traffic light says whether anything is
// happening, and the goals are checkboxes with nothing hidden behind a flag.
type desktop struct {
	app  *gw.App
	sup  *supervisor.Supervisor
	pins *pins.Pins
	jour *journal.Journal

	win      *gw.Window
	lamp     *gw.Label
	detail   *gw.Label
	counts   *gw.Label
	pauseBtn *gw.Button
	log      *gw.TextView
	tray     *gw.TrayIcon
	hasTray  bool

	mu     sync.Mutex
	cancel context.CancelFunc
}

func runDesktop(codexPath, prompt string, verbose bool) error {
	client, path, err := connect(codexPath, verbose)
	if err != nil {
		return err
	}
	jour, err := journal.Open()
	if err != nil {
		client.Close()
		return err
	}
	client.SetActivitySink(func(a appserver.Activity) { jour.Record(a) })

	thePins := pins.Load()
	app, err := gw.NewApp()
	if err != nil {
		client.Close()
		jour.Close()
		return fmt.Errorf("графическая подсистема недоступна: %w", err)
	}

	d := &desktop{app: app, pins: thePins, jour: jour}
	d.sup = supervisor.New(supervisor.Options{
		Client: client, Pins: thePins, Journal: jour, Prompt: prompt,
		OnChange: func(s supervisor.Status) {
			app.QueueUpdate(func() { d.render(s) })
		},
	})

	fmt.Println("codex:", path)
	fmt.Println("журнал:", jour.Path())

	if err := d.build(); err != nil {
		client.Close()
		jour.Close()
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	go func() { _ = d.sup.Run(ctx) }()
	go d.pollLog()

	err = app.Run(d.win)
	cancel()
	client.Close()
	jour.Close()
	return err
}

func (d *desktop) build() error {
	win, err := d.app.NewWindow("crescent", 660, 700)
	if err != nil {
		return err
	}
	d.win = win

	// The traffic light first, on its own line, in its own words. The previous
	// window put the state in ordinary text right above the checkboxes, where
	// it read as another list item and could not be found at a glance.
	d.lamp, _ = win.AddLabel("")
	d.detail, _ = win.AddLabel("")
	d.counts, _ = win.AddLabel("")
	_, _ = win.AddLabel("────────────────────────────────────────────")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	goals, err := d.sup.Goals(ctx)
	if err != nil {
		goals = nil
	}

	// Every goal gets a row. Hiding some behind "and N more, use another
	// command" put the goals this exists for out of reach; duplicates are gone
	// now, so the list is short enough to show whole.
	for _, g := range goals {
		g := g
		label := g.Label
		if g.Status != "" {
			label += "  [" + g.Status + "]"
		}
		box, err := win.AddCheckBox(label, d.pins.Pinned(g.ThreadID))
		if err != nil {
			return err
		}
		box.Toggled.On(d.app.Scope(), func(on bool) {
			d.pins.Set(g.ThreadID, g.Label, on)
			verb := "снята с наблюдения"
			if on {
				verb = "закреплена — будет перезапускаться после сброса лимита"
			}
			d.jour.Note("", g.Label+": "+verb)
			d.sup.Wake()
			d.render(d.sup.Status())
		})
	}
	if len(goals) == 0 {
		_, _ = win.AddLabel("Целей не найдено. Проверьте, что Codex залогинен.")
	}

	_, _ = win.AddLabel("Журнал:")
	d.log, _ = win.AddTextView(150)

	d.pauseBtn, _ = win.AddButton("Пауза")
	d.pauseBtn.Clicked.On(d.app.Scope(), func(gw.ClickInfo) {
		paused := !d.sup.Paused()
		d.sup.SetPaused(paused)
		if paused {
			d.jour.Note("", "пауза — перезапуски остановлены")
		} else {
			d.jour.Note("", "продолжаем")
		}
		d.render(d.sup.Status())
	})

	logBtn, _ := win.AddButton("Открыть папку журналов")
	logBtn.Clicked.On(d.app.Scope(), func(gw.ClickInfo) { openPath(d.jour.Path()) })

	hideBtn, _ := win.AddButton("Свернуть в трей")
	hideBtn.Clicked.On(d.app.Scope(), func(gw.ClickInfo) { d.win.Hide() })

	quitBtn, _ := win.AddButton("Выйти (остановить наблюдение)")
	quitBtn.Clicked.On(d.app.Scope(), func(gw.ClickInfo) { d.quit() })

	// Breathing room under the last button: it sat flush against the window
	// edge and looked cut off.
	_, _ = win.AddLabel(" ")

	d.buildTray()

	win.Closing.On(d.app.Scope(), func(req *gw.CloseRequest) {
		if d.hasTray {
			req.CancelClose()
			d.win.Hide()
			d.jour.Note("", "окно свёрнуто в трей — наблюдение продолжается")
			return
		}
		d.quit()
	})

	d.render(d.sup.Status())
	d.refreshLog()
	return nil
}

func (d *desktop) buildTray() {
	show := gw.NewMenuItem("Показать окно")
	openLog := gw.NewMenuItem("Открыть папку журналов")
	quit := gw.NewMenuItem("Выйти")
	tray, err := d.app.NewTrayIcon("crescent", show, openLog, gw.Separator(), quit)
	if err != nil {
		return
	}
	d.tray, d.hasTray = tray, true
	show.Clicked.On(d.app.Scope(), func(struct{}) { d.win.Show() })
	openLog.Clicked.On(d.app.Scope(), func(struct{}) { openPath(d.jour.Path()) })
	quit.Clicked.On(d.app.Scope(), func(struct{}) { d.quit() })
	tray.Activated.On(d.app.Scope(), func(struct{}) { d.win.Show() })
}

func (d *desktop) render(s supervisor.Status) {
	lamp := s.Lamp()
	d.lamp.Text.Set(lamp.Symbol() + "   " + lamp.Word())
	d.detail.Text.Set(s.Line())
	d.counts.Text.Set(fmt.Sprintf("Отмечено целей: %d   •   перезапусков: %d",
		d.pins.Count(), s.Restarts))
	if d.pauseBtn != nil {
		if d.sup.Paused() {
			d.pauseBtn.Text.Set("Продолжить")
		} else {
			d.pauseBtn.Text.Set("Пауза")
		}
	}
	if d.hasTray {
		d.tray.Tooltip.Set("crescent " + lamp.Symbol() + " " + s.Line())
	}
}

func (d *desktop) pollLog() {
	for range time.Tick(700 * time.Millisecond) {
		d.app.QueueUpdate(d.refreshLog)
	}
}

func (d *desktop) refreshLog() {
	if d.log != nil {
		d.log.SetText(d.jour.Tail(200))
	}
}

func (d *desktop) quit() {
	d.mu.Lock()
	if d.cancel != nil {
		d.cancel()
	}
	d.mu.Unlock()
	d.app.Quit()
}

func openPath(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	_ = cmd.Start()
}
