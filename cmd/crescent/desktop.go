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

// desktop is the whole application with a face: pick goals to keep alive by
// ticking them, and the supervisor keeps them moving across limit resets. Close
// the window and it lives in the tray; the work carries on behind it.
type desktop struct {
	app  *gw.App
	sup  *supervisor.Supervisor
	pins *pins.Pins
	jour *journal.Journal

	win     *gw.Window
	status  *gw.Label
	counts  *gw.Label
	tray    *gw.TrayIcon
	hasTray bool

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
	// Route the model's activity into the journal as it streams in.
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
		Client:  client,
		Pins:    thePins,
		Journal: jour,
		Prompt:  prompt,
		OnChange: func(s supervisor.Status) {
			// The supervisor runs on its own goroutine; hop onto the UI thread.
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

	err = app.Run(d.win)
	cancel()
	client.Close()
	jour.Close()
	return err
}

func (d *desktop) build() error {
	win, err := d.app.NewWindow("crescent", 620, 460)
	if err != nil {
		return err
	}
	d.win = win

	d.status, _ = win.AddLabel("Загружаю цели…")
	d.counts, _ = win.AddLabel("")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	goals, err := d.sup.Goals(ctx)
	if err != nil {
		d.status.Text.Set("Не удалось получить цели: " + err.Error())
		goals = nil
	}

	// One checkbox per goal. Ticking pins it — kept across restarts — and the
	// supervisor picks it up on its next pass without anything being restarted
	// now.
	shown := goals
	if len(shown) > 14 {
		shown = shown[:14] // a plain stack shows only so many honestly
	}
	for _, g := range shown {
		g := g
		label := fmt.Sprintf("%s  [%s]", g.Label, g.Status)
		box, err := win.AddCheckBox(label, d.pins.Pinned(g.ThreadID))
		if err != nil {
			return err
		}
		box.Toggled.On(d.app.Scope(), func(on bool) {
			d.pins.Set(g.ThreadID, g.Label, on)
			d.render(d.sup.Status())
		})
	}
	if len(goals) == 0 {
		if _, err := win.AddLabel("Целей не найдено. Проверьте, что Codex залогинен."); err != nil {
			return err
		}
	}

	logBtn, _ := win.AddButton("Открыть журнал")
	logBtn.Clicked.On(d.app.Scope(), func(gw.ClickInfo) { openPath(d.jour.Path()) })

	hideBtn, _ := win.AddButton("Свернуть в трей")
	hideBtn.Clicked.On(d.app.Scope(), func(gw.ClickInfo) { d.win.Hide() })

	quitBtn, _ := win.AddButton("Выйти (остановить наблюдение)")
	quitBtn.Clicked.On(d.app.Scope(), func(gw.ClickInfo) { d.quit() })

	d.buildTray()

	// Closing the window hides it to the tray — the work keeps running. Only
	// when there is no tray does closing mean quitting, since otherwise there
	// would be no way back.
	win.Closing.On(d.app.Scope(), func(req *gw.CloseRequest) {
		if d.hasTray {
			req.CancelClose()
			d.win.Hide()
			return
		}
		d.quit()
	})

	d.render(d.sup.Status())
	return nil
}

func (d *desktop) buildTray() {
	show := gw.NewMenuItem("Показать окно")
	openLog := gw.NewMenuItem("Открыть журнал")
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

// render reflects a status snapshot into the window and the tray tooltip.
func (d *desktop) render(s supervisor.Status) {
	line := s.Line()
	d.status.Text.Set(line)
	d.counts.Text.Set(fmt.Sprintf("Закреплено целей: %d  •  перезапусков: %d",
		d.pins.Count(), s.Restarts))
	if d.hasTray {
		d.tray.Tooltip.Set("crescent — " + line)
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

// openPath opens a folder in the system file manager, best-effort.
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
