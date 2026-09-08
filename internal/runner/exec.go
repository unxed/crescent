package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/unxed/crescent/internal/codex"
)

// Options tune one supervised run.
type Options struct {
	// ReadOnly forces --sandbox read-only. The first run against a real
	// session should use it: it proves that resume, authentication and the
	// event stream all work, without any chance of touching a repository.
	ReadOnly bool

	// RawTo receives every line of the stream verbatim. The event schema is
	// undocumented, so a raw capture is how the parser gets corrected.
	RawTo io.Writer

	// Trace receives a short human-readable line per event.
	Trace io.Writer

	// Timeout bounds a single turn.
	Timeout time.Duration
}

// Result is what one turn did.
type Result struct {
	Command      string
	ExitCode     int
	Duration     time.Duration
	Lines        int
	Parsed       int
	UsageLimited bool
	ResetsAt     time.Time
	LastMessage  string
	Errors       []string
	EventTypes   map[string]int
}

// RunOnce resumes a session for exactly one turn and reports what happened.
//
// It never loops and never decides on its own to run again: continuing after a
// usage-limit reset is a separate decision, made with the whole queue in view.
func RunOnce(ctx context.Context, p Policy, s codex.Session, o Options) (Result, error) {
	res := Result{EventTypes: map[string]int{}}

	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}

	pol := p
	if o.ReadOnly {
		pol.Sandbox = "read-only"
		pol.WritableRoots = nil
	}
	args := BuildArgs(s, pol)
	res.Command = pol.CodexPath + " " + strings.Join(args, " ")

	cmd := exec.CommandContext(ctx, pol.CodexPath, args...)
	cmd.Dir = s.Cwd
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	cmd.Stderr = os.Stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("не удалось запустить %s: %w", pol.CodexPath, err)
	}

	sc := bufio.NewScanner(stdout)
	// Event payloads can carry a whole file; the default 64 KiB limit would
	// truncate them into invalid JSON and lose the rest of the turn.
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)

	for sc.Scan() {
		line := sc.Text()
		res.Lines++
		if o.RawTo != nil {
			fmt.Fprintln(o.RawTo, line)
		}
		res.absorb(line, o.Trace, start)
	}
	waitErr := cmd.Wait()
	res.Duration = time.Since(start)

	if ee, ok := waitErr.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
	} else if waitErr != nil {
		return res, waitErr
	}
	return res, nil
}

// absorb classifies one line of the stream. Like the rollout parser, it does
// not assume a schema: it walks whatever JSON arrived and recognises keys it
// knows, so a renamed wrapper does not blind it.
func (r *Result) absorb(line string, trace io.Writer, start time.Time) {
	var v any
	if json.Unmarshal([]byte(line), &v) != nil {
		return
	}
	r.Parsed++

	var kind, text, errText string
	codex.Walk(v, func(key string, val any) {
		s, isStr := val.(string)
		switch strings.ToLower(strings.ReplaceAll(key, "_", "")) {
		case "type", "msgtype", "event":
			if isStr && kind == "" {
				kind = s
			}
		case "text", "message", "delta", "content":
			if isStr && s != "" && len(s) > len(text) {
				text = s
			}
		case "error", "errormessage":
			if isStr && s != "" {
				errText = s
			}
		case "codexerrorinfo":
			if isStr {
				errText = s
			}
		case "resetsat", "resetat":
			if t, ok := codex.ParseTime(val); ok {
				r.ResetsAt = t
			}
		}
	})

	if kind != "" {
		r.EventTypes[kind]++
	}
	if text != "" {
		r.LastMessage = text
	}

	// A usage limit is the one outcome crescent exists for, so it is detected
	// from the whole line rather than from one field: the wrapper around the
	// message has changed before and will again.
	low := strings.ToLower(line)
	for _, marker := range []string{"usagelimit", "usage limit", "ratelimitreached", "rate_limit_reached", "you've hit your usage limit"} {
		if strings.Contains(low, marker) {
			r.UsageLimited = true
			break
		}
	}
	if errText != "" {
		r.Errors = append(r.Errors, errText)
	}

	if trace != nil && kind != "" {
		fmt.Fprintf(trace, "  [%6.1fs] %-28s %s\n",
			time.Since(start).Seconds(), kind, oneLine(text, 70))
	}
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
