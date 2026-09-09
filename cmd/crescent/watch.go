package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/unxed/crescent/internal/appserver"
	"github.com/unxed/crescent/internal/journal"
	"github.com/unxed/crescent/internal/pins"
	"github.com/unxed/crescent/internal/supervisor"
)

// runWatch is the terminal face of the same supervisor the window drives. It
// pins the named goals (or uses whatever is already pinned), then hands over to
// the supervisor and prints its status changes. Ctrl-C stops watching; the
// goals themselves keep whatever state they were in.
func runWatch(codexPath string, selectors []string, prompt string, verbose bool, poll time.Duration) error {
	lock, err := holdSingleInstance()
	if err != nil {
		return err
	}
	defer lock.Release()

	client, path, err := connect(codexPath, verbose, false)
	if err != nil {
		return err
	}
	defer client.Close()

	jour, err := journal.Open()
	if err != nil {
		return err
	}
	defer jour.Close()
	client.SetActivitySink(func(a appserver.Activity) { jour.Record(a) })

	thePins := pins.Load()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// If goals were named, they replace the pinned set for this run; naming
	// them is the whole instruction. With no names, we watch what is already
	// pinned — the window's selection, honoured from the terminal.
	if len(selectors) > 0 {
		cands, err := client.Candidates(ctx)
		if err != nil {
			return err
		}
		chosen, err := resolve(cands, selectors)
		if err != nil {
			return err
		}
		for _, c := range chosen {
			thePins.Set(c.Thread.ID, c.Thread.Label(), true)
		}
	}
	if thePins.Count() == 0 {
		return fmt.Errorf("ни одна цель не закреплена; назовите их: crescent -watch \"Konsole,Лунобот\"")
	}

	fmt.Println("codex: ", path)
	fmt.Println("журнал:", jour.Path())
	fmt.Printf("\nВеду %d целей:\n", thePins.Count())
	for _, id := range thePins.IDs() {
		fmt.Printf("  • %s\n", thePins.Label(id))
	}
	fmt.Println("\nCtrl-C — остановить наблюдение.")

	sup := supervisor.New(supervisor.Options{
		Client: client, Pins: thePins, Journal: jour, Prompt: prompt, Poll: poll,
		OnChange: func(s supervisor.Status) {
			fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), s.Line())
		},
	})

	client.SetStderrSink(func(line string) {
		jour.Server(line)
		sup.NoteServerLine(line)
	})

	sigctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return sup.Run(sigctx)
}

// resolve turns selectors into goals: a number is a position in the candidate
// list, anything else matches the chat name or goal text and takes every match
// (several goals share a name here).
func resolve(all []appserver.Candidate, selectors []string) ([]appserver.Candidate, error) {
	seen := map[string]bool{}
	var out []appserver.Candidate
	for _, sel := range selectors {
		if n, err := strconv.Atoi(sel); err == nil {
			if n < 1 || n > len(all) {
				return nil, fmt.Errorf("номер %d вне списка (целей: %d)", n, len(all))
			}
			add(&out, seen, all[n-1])
			continue
		}
		low := strings.ToLower(sel)
		found := 0
		for _, c := range all {
			if strings.Contains(strings.ToLower(c.Thread.Label()), low) ||
				strings.Contains(strings.ToLower(c.Goal.Objective), low) {
				add(&out, seen, c)
				found++
			}
		}
		if found == 0 {
			return nil, fmt.Errorf("по %q ничего не нашлось; список: crescent -restart -dry-run", sel)
		}
	}
	return out, nil
}

func add(out *[]appserver.Candidate, seen map[string]bool, c appserver.Candidate) {
	if !seen[c.Thread.ID] {
		seen[c.Thread.ID] = true
		*out = append(*out, c)
	}
}
