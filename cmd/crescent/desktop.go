package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
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
	"github.com/unxed/winkeys"
)

// desktop is crescent with a face. The design target is a tired person at four
// in the morning: one glance at the traffic light says whether anything is
// happening, and the goals are checkboxes with nothing hidden behind a flag.
type desktop struct {
	app    *gw.App
	client *appserver.Client
	sup    *supervisor.Supervisor
	pins   *pins.Pins
	jour   *journal.Journal

	win     *gw.Window
	lamp    *gw.Label
	detail  *gw.Label
	counts  *gw.Label
	limits  *gw.Label
	limits2 *gw.Label
	// autoOK is the standing answer to Codex asking permission. On by default:
	// a person starts crescent to walk away, and a prompt nobody answers stops
	// the work as surely as an error.
	autoOK     bool
	pending    []pendingApproval
	pauseBtn   *gw.Button
	approveBtn *gw.Button
	viewBtn    *gw.Button
	log        *gw.TextView

	// which journal the text view is showing: "" is the combined feed, any
	// other value is one goal's own log.
	viewing string
	tray    *gw.TrayIcon
	hasTray bool

	// rows lets the counters beside each goal be refreshed in place.
	rows []goalRow

	mu     sync.Mutex
	cancel context.CancelFunc
}

func runDesktop(codexPath, prompt string, verbose, wire, trace, serverLog bool, poll time.Duration) error {
	// One crescent at a time. Two of them take turns on the same threads and
	// each sees the other's turn as "already has an active writer".
	lock, err := holdSingleInstance()
	if err != nil {
		return err
	}
	defer lock.Release()

	client, path, err := connect(codexPath, verbose, true)
	if err != nil {
		return err
	}
	jour, err := journal.Open()
	if err != nil {
		client.Close()
		return err
	}
	// Installed below, once the supervisor exists: a line naming a thread has
	// to reach both the journal and the supervisor, which decides whether the
	// goal is worth restarting at all.

	jour.ShowServerLines(serverLog)

	thePins := pins.Load()
	app, err := gw.NewApp()
	if err != nil {
		client.Close()
		jour.Close()
		return fmt.Errorf("графическая подсистема недоступна: %w", err)
	}

	d := &desktop{app: app, pins: thePins, jour: jour, client: client, autoOK: true}
	d.sup = supervisor.New(supervisor.Options{
		Client: client, Pins: thePins, Journal: jour, Prompt: prompt, Poll: poll,
		OnChange: func(s supervisor.Status) {
			app.QueueUpdate(func() { d.render(s) })
		},
	})

	// Installed once the supervisor exists: the turn id appears only in this
	// stream, and turn/interrupt cannot be issued without it.
	if wire {
		if path, err := jour.OpenWire(); err == nil {
			fmt.Println("протокол пишется дословно:", path)
			client.SetWireSink(jour.Wire)
		} else {
			fmt.Println("не удалось открыть запись протокола:", err)
		}
	}
	client.SetRequestHandler(d.onServerRequest)
	client.SetStderrSink(func(line string) {
		jour.Server(line)
		d.sup.NoteServerLine(line)
	})
	client.SetActivitySink(func(a appserver.Activity) {
		jour.Record(a)
		d.sup.NoteTurn(a.ThreadID, a.TurnID)
		d.sup.NoteActivity(a.ThreadID)
	})

	fmt.Println("codex:", path)
	fmt.Println("журнал:", jour.Path())

	if err := d.build(); err != nil {
		client.Close()
		jour.Close()
		return err
	}

	if trace {
		if path, err := jour.OpenTrace(); err == nil {
			fmt.Println("решения пишутся:", path)
			d.sup.SetTrace(jour.Trace)
		} else {
			fmt.Println("не удалось открыть запись решений:", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	go func() { _ = d.sup.Run(ctx) }()
	go d.pollLog()

	// Every reason the loop can end is recorded before anything is torn down.
	// A live run had app-server exit after 33 seconds with
	// exit_reason="stdio_connection_closed" — that is our end of the pipe
	// closing, which happens here and nowhere else, and the journal said
	// nothing about why we got here.
	jour.Note("", "цикл окна работает")
	err = app.Run(d.win)
	if err != nil {
		jour.Note("", "цикл окна завершился с ошибкой: "+err.Error())
	} else {
		jour.Note("", "цикл окна завершился без ошибки — окно закрыто или Quit")
	}
	jour.Note("", "закрываю app-server и журнал")

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
	d.limits, _ = win.AddLabel("")
	d.limits2, _ = win.AddLabel("")
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
		// Register the name now so the journal switcher can offer this goal
		// even before anything has been written for it.
		d.jour.Label(g.ThreadID, g.Label)
		label := g.Label
		if g.Status != "" {
			label += "  [" + g.Status + "]"
		}
		box, err := win.AddCheckBox(label, d.pins.Pinned(g.ThreadID))
		if err != nil {
			return err
		}
		// Registered so the row can carry live counters and the reason it is
		// waiting. The list was declared and iterated but never filled, so every
		// per-goal number silently never appeared.
		d.rows = append(d.rows, goalRow{id: g.ThreadID, label: g.Label, box: box})
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

	// Pins with no goal behind them still get a row. Without one they were
	// counted but invisible: the window said four goals were pinned while
	// showing three checkboxes, and the fourth could not be unpinned at all
	// because there was nothing to click.
	shown := map[string]bool{}
	for _, g := range goals {
		shown[g.ThreadID] = true
	}
	for _, id := range d.pins.IDs() {
		if shown[id] {
			continue
		}
		id, label := id, d.pins.Label(id)
		box, err := win.AddCheckBox(label+"  — чат не найден, снимите галочку", true)
		if err != nil {
			return err
		}
		box.Toggled.On(d.app.Scope(), func(on bool) {
			if on {
				return // re-pinning a thread that does not exist helps nobody
			}
			d.pins.Set(id, label, false)
			d.jour.Note("", label+": снята — чата с таким идентификатором нет")
			d.render(d.sup.Status())
		})
	}

	// A button that cycles the journal rather than a list of them: with only
	// checkboxes and buttons available, one control that names what it will
	// show next is clearer than a row of per-goal buttons that grows with the
	// list.
	d.viewBtn, _ = win.AddButton("")
	d.viewBtn.Clicked.On(d.app.Scope(), func(gw.ClickInfo) { d.cycleView() })
	d.log, _ = win.AddTextView(150)

	// On by default, and stated plainly: this is what makes an unattended run
	// unattended. Codex asks even where its own settings already allow the
	// action, so the prompts it raises here are its caution, not new authority.
	autoBox, err := win.AddCheckBox("Подтверждать запросы Codex автоматически", true)
	if err != nil {
		return err
	}
	autoBox.Toggled.On(d.app.Scope(), func(on bool) {
		d.mu.Lock()
		d.autoOK = on
		d.mu.Unlock()
		if on {
			d.jour.Note("", "подтверждаю запросы Codex автоматически")
			d.approveAll() // anything already waiting is answered now
		} else {
			d.jour.Note("", "запросы Codex буду показывать вам")
		}
	})

	d.approveBtn, _ = win.AddButton("Подтвердить запросы")
	d.approveBtn.Clicked.On(d.app.Scope(), func(gw.ClickInfo) { d.approveAll() })

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

	hideBtn, _ := win.AddButton("Свернуть в трей  (или Esc)")
	hideBtn.Clicked.On(d.app.Scope(), func(gw.ClickInfo) { d.hideToTray() })

	// No quit button. Closing, Esc and Alt+F4 all go to the tray, so the only
	// way to actually stop is the tray menu — a deliberate act, not something
	// a stray click can do while goals are running.
	_, _ = win.AddLabel("Выход — правой кнопкой по иконке в трее.")

	// Breathing room under the last button: it sat flush against the window
	// edge and looked cut off.
	_, _ = win.AddLabel(" ")

	d.buildTray()

	// Esc hides the window, the same as the close button and Alt+F4. Alt+F4
	// reaches us as an ordinary close request from the window manager, so it
	// needs nothing of its own.
	win.KeyPressed().On(d.app.Scope(), func(k gw.Key) {
		if k.KeyDown && k.VirtualKeyCode == winkeys.VK_ESCAPE {
			d.hideToTray()
		}
	})

	win.Closing.On(d.app.Scope(), func(req *gw.CloseRequest) {
		// Logged on arrival: on one machine the close button quits instead of
		// hiding, and this line distinguishes "the request never reached us"
		// from "it reached us and we decided wrongly".
		d.jour.Note("", fmt.Sprintf("запрос закрытия окна получен (трей: %v)", d.hasTray))
		if d.hasTray {
			req.CancelClose()
			d.hideToTray()
			return
		}
		// Without a tray there would be no way back, so closing has to mean
		// quitting; anything else would lose the window for good.
		d.quit()
	})

	d.render(d.sup.Status())
	d.refreshLog()
	return nil
}

// noteTurn forwards a turn id to the supervisor once it exists.
func (d *desktop) noteTurn(threadID, turnID string) {
	if d.sup != nil {
		d.sup.NoteTurn(threadID, turnID)
	}
}

// goalRow ties a checkbox to the goal it stands for, so its label can carry
// live counters.
type goalRow struct {
	id, label string
	box       *gw.CheckBox
}

// pendingApproval is a request waiting for a person, kept only when automatic
// approval is switched off.
type pendingApproval struct {
	id      int
	method  string
	summary string
	params  json.RawMessage
}

// onServerRequest answers Codex when it asks permission.
func (d *desktop) onServerRequest(id int, method string, params json.RawMessage) {
	if !appserver.IsApprovalRequest(method) {
		return // some other request; nothing sensible to answer
	}
	summary := appserver.ApprovalSummary(method, params)

	d.mu.Lock()
	auto := d.autoOK
	if !auto {
		d.pending = append(d.pending, pendingApproval{id, method, summary, params})
	}
	d.mu.Unlock()

	if !auto {
		d.jour.Note("", "Codex просит разрешения: "+summary+" — нажмите «Подтвердить»")
		d.app.QueueUpdate(func() { d.render(d.sup.Status()) })
		return
	}
	if err := d.client.Respond(id, appserver.ApprovalAnswer(method, params)); err != nil {
		d.jour.Note("", "не удалось ответить на запрос разрешения: "+err.Error())
		return
	}
	d.jour.Note("", "разрешено автоматически: "+summary)
}

// approveAll answers everything that is waiting.
func (d *desktop) approveAll() {
	d.mu.Lock()
	waiting := d.pending
	d.pending = nil
	d.mu.Unlock()

	for _, p := range waiting {
		if err := d.client.Respond(p.id, appserver.ApprovalAnswer(p.method, p.params)); err != nil {
			d.jour.Note("", "не удалось подтвердить: "+err.Error())
			continue
		}
		d.jour.Note("", "разрешено вами: "+p.summary)
	}
	d.render(d.sup.Status())
}

func (d *desktop) buildTray() {
	show := gw.NewMenuItem("Показать окно")
	openLog := gw.NewMenuItem("Открыть папку журналов")
	quit := gw.NewMenuItem("Выйти")
	tray, err := d.app.NewTrayIcon("crescent", show, openLog, gw.Separator(), quit)
	if err != nil {
		// Recorded, because whether a tray exists decides what the close button
		// does: with one, closing hides; without one it must quit, or the
		// window would be lost with no way back.
		d.jour.Note("", "трей недоступен ("+err.Error()+") — закрытие окна будет означать выход")
		return
	}
	d.jour.Note("", "иконка в трее создана — закрытие окна будет сворачивать")
	d.tray, d.hasTray = tray, true
	show.Clicked.On(d.app.Scope(), func(struct{}) { d.win.Show() })
	openLog.Clicked.On(d.app.Scope(), func(struct{}) { openPath(d.jour.Path()) })
	quit.Clicked.On(d.app.Scope(), func(struct{}) { d.quit() })
	tray.Activated.On(d.app.Scope(), func(struct{}) { d.win.Show() })
}

func (d *desktop) render(s supervisor.Status) {
	// The lamp is recomputed here, from evidence and the current time, rather
	// than taken from whatever the supervisor last reported. That is the whole
	// point: if the supervisor stops updating — hung, crashed, deadlocked —
	// nobody is left to set the lamp red, so the lamp has to go red by itself.
	lamp := s.Lamp()
	d.lamp.Text.Set(lamp.Symbol() + "   " + lamp.Word() + " — " + s.Why())
	d.detail.Text.Set(s.Line())
	counts := fmt.Sprintf("Отмечено целей: %d   •   перезапусков: %d   •   потрачено токенов: %d",
		d.pins.Count(), s.Restarts, s.SpentSince)
	if !s.NextPass.IsZero() {
		if left := time.Until(s.NextPass); left > 0 {
			counts += fmt.Sprintf("   •   следующая проверка через %s", left.Round(time.Second))
		} else {
			counts += "   •   проверяю…"
		}
	}
	if !s.Until.IsZero() {
		if left := time.Until(s.Until); left > 0 {
			counts += fmt.Sprintf("   •   сброс лимита через %s", left.Round(time.Minute))
		}
	}
	d.counts.Text.Set(counts)
	// Split across two lines: one long line of limits stretched the window
	// wider than everything else in it.
	switch {
	case len(s.Limits) == 0:
		d.limits.Text.Set("Лимиты: ещё не прочитаны")
		d.limits2.Text.Set("")
	default:
		half := (len(s.Limits) + 1) / 2
		d.limits.Text.Set("Лимиты:  " + strings.Join(s.Limits[:half], "   |   "))
		if half < len(s.Limits) {
			d.limits2.Text.Set("         " + strings.Join(s.Limits[half:], "   |   "))
		} else {
			d.limits2.Text.Set("")
		}
	}
	if d.approveBtn != nil {
		d.mu.Lock()
		n := len(d.pending)
		d.mu.Unlock()
		switch {
		case n == 0:
			d.approveBtn.Text.Set("Подтверждать нечего")
		case n == 1:
			d.approveBtn.Text.Set("Подтвердить 1 запрос Codex")
		default:
			d.approveBtn.Text.Set(fmt.Sprintf("Подтвердить запросы Codex: %d", n))
		}
	}
	if d.pauseBtn != nil {
		if d.sup.Paused() {
			d.pauseBtn.Text.Set("Продолжить")
		} else {
			d.pauseBtn.Text.Set("Пауза")
		}
	}
	// Each goal shows its own token counter — the per-goal tokensUsed from
	// thread/goal/get, which is a different number from the account limits
	// above and the one the lamp is judged by.
	for _, r := range d.rows {
		text := r.label
		if sp, ok := s.PerGoal[r.id]; ok {
			text += fmt.Sprintf("  [%s]  %d токенов", sp.Status, sp.Tokens)
			// Whatever this goal is waiting for, said in words, with the time
			// left when there is one. Silence during a wait was the single
			// thing that made the program look broken while it worked.
			if sp.Waiting != "" {
				text += "  — " + sp.Waiting
				if !sp.Until.IsZero() {
					if left := time.Until(sp.Until); left > 0 {
						text += fmt.Sprintf(", ещё %s", left.Round(time.Second))
					}
				}
			}
		}
		r.box.Text.Set(text)
	}

	if d.hasTray {
		d.tray.Tooltip.Set("crescent " + lamp.Symbol() + " " + s.Line())
	}
}

func (d *desktop) pollLog() {
	for range time.Tick(700 * time.Millisecond) {
		d.app.QueueUpdate(func() {
			d.refreshLog()
			// Re-render on every tick so the lamp ages towards red on its own
			// even when no status update ever arrives.
			d.render(d.sup.Status())
		})
	}
}

// cycleView moves the journal pane to the next goal, wrapping back to the
// combined feed.
func (d *desktop) cycleView() {
	views := d.jour.Views()
	at := 0
	for i, v := range views {
		if v == d.viewing {
			at = i
			break
		}
	}
	d.viewing = views[(at+1)%len(views)]
	d.refreshLog()
}

func (d *desktop) refreshLog() {
	if d.log == nil {
		return
	}
	text := d.jour.TailOf(d.viewing, 400)
	if text == "" {
		if d.viewing == "" {
			text = "(пока пусто)"
		} else {
			text = "(для этой цели записей ещё нет — они появятся, когда её перезапустят)"
		}
	}
	d.log.SetText(text)
	if d.viewBtn != nil {
		name := "все цели вместе"
		if d.viewing != "" {
			name = d.viewing
		}
		d.viewBtn.Text.Set("Журнал: " + name + "   (нажмите, чтобы переключить)")
	}
}

// hideToTray puts the window away without stopping anything.
func (d *desktop) hideToTray() {
	if !d.hasTray {
		return // nowhere to hide to
	}
	d.win.Hide()
	d.jour.Note("", "окно свёрнуто в трей — наблюдение продолжается")
}

func (d *desktop) quit() {
	d.jour.Note("", "запрошен выход (Quit)")
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
