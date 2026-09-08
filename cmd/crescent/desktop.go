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

// desktop is the whole application with a face: tick the goals to keep alive,
// watch what they do in the log below, close the window and it lives in the
// tray while the work goes on.
type desktop struct {
	app  *gw.App
	sup  *supervisor.Supervisor
	pins *pins.Pins
	jour *journal.Journal

	win     *gw.Window
	status  *gw.Label
	counts  *gw.Label
	log     *gw.TextView
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

	// The log is a file the supervisor and the activity sink write to; poll it
	// a couple of times a second so the window shows what is happening live.
	go d.pollLog()

	err = app.Run(d.win)
	cancel()
	client.Close()
	jour.Close()
	return err
}

func (d *desktop) build() error {
	// Tall enough for the goal list, the log, and every button — the previous
	// window cut the last button off the bottom.
	win, err := d.app.NewWindow("crescent", 640, 640)
	if err != nil {
		return err
	}
	d.win = win

	d.status, _ = win.AddLabel("")
	d.counts, _ = win.AddLabel("")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	goals, err := d.sup.Goals(ctx)
	if err != nil {
		goals = nil
	}

	// The goal list scrolls in its own text view is not the answer here — these
	// are checkboxes. So the list is capped and, above the cap, folded into a
	// note; the log stays a fixed-height pane so the window never grows past the
	// buttons no matter how much happens.
	shown := goals
	const maxRows = 8
	if len(shown) > maxRows {
		shown = shown[:maxRows]
	}
	for _, g := range shown {
		g := g
		box, err := win.AddCheckBox(fmt.Sprintf("%s  [%s]", g.Label, g.Status), d.pins.Pinned(g.ThreadID))
		if err != nil {
			return err
		}
		box.Toggled.On(d.app.Scope(), func(on bool) {
			d.pins.Set(g.ThreadID, g.Label, on)
			// Immediate feedback: say what happened, and wake the supervisor so
			// the effect is not up to a poll interval away.
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
	} else if len(goals) > len(shown) {
		_, _ = win.AddLabel(fmt.Sprintf("…и ещё %d — отметить их можно через crescent -watch",
			len(goals)-len(shown)))
	}

	// The log, in the window. This is the whole point of the run: seeing what
	// the model does while it works unattended.
	_, _ = win.AddLabel("Журнал:")
	d.log, _ = win.AddTextView(170)

	logBtn, _ := win.AddButton("Открыть папку журналов")
	logBtn.Clicked.On(d.app.Scope(), func(gw.ClickInfo) { openPath(d.jour.Path()) })

	hideBtn, _ := win.AddButton("Свернуть в трей")
	hideBtn.Clicked.On(d.app.Scope(), func(gw.ClickInfo) { d.win.Hide() })

	quitBtn, _ := win.AddButton("Выйти (остановить наблюдение)")
	quitBtn.Clicked.On(d.app.Scope(), func(gw.ClickInfo) { d.quit() })

	d.buildTray()

	// The close button hides to the tray, so a stray click never loses the run.
	// Only without a tray does closing quit, since then there is no way back.
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

// render reflects a status snapshot into the window and the tray tooltip.
func (d *desktop) render(s supervisor.Status) {
	line := s.Line()
	d.status.Text.Set("Состояние: " + line)
	d.counts.Text.Set(fmt.Sprintf("Закреплено целей: %d  •  перезапусков: %d",
		d.pins.Count(), s.Restarts))
	if d.hasTray {
		d.tray.Tooltip.Set("crescent — " + line)
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
