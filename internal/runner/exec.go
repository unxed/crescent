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
	Failure      Failure
	ResetsAt     time.Time
	LastMessage  string
	Errors       []string
	EventTypes   map[string]int

	// Tokens from turn.completed, for a report that says what the turn cost.
	InputTokens, OutputTokens int64
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

// absorb classifies one line of the stream.
//
// The event types are known exactly (see events.go), so the common shapes are
// read directly. The tolerant walk is kept underneath as a safety net: the
// schema has changed before, and a renamed wrapper should degrade the report,
// not blind it.
func (r *Result) absorb(line string, trace io.Writer, start time.Time) {
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
		Usage   struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
		Item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil {
		return
	}
	r.Parsed++

	kind := ev.Type
	var text string

	switch ev.Type {
	case "turn.completed":
		r.InputTokens += ev.Usage.InputTokens
		r.OutputTokens += ev.Usage.OutputTokens

	case "turn.failed", "error":
		msg := ev.Error.Message
		if msg == "" {
			msg = ev.Message
		}
		if msg != "" {
			r.Errors = append(r.Errors, msg)
			r.LastMessage = msg
		}
		if f := classifyFailure(msg); f != FailNone {
			r.Failure = f
			r.UsageLimited = f == FailUsageLimit
			if at, ok := parseRetryAt(msg, time.Now()); ok {
				r.ResetsAt = at
			}
		}

	case "item.started", "item.updated", "item.completed":
		if ev.Item.Type != "" {
			kind = ev.Type + ":" + ev.Item.Type
		}
		text = ev.Item.Text
		if ev.Item.Type == ItemAgentMessage && text != "" {
			r.LastMessage = text
		}
	}

	// Safety net for a schema that moves: pick up a reset timestamp or a
	// usage-limit wording wherever they might appear.
	if r.ResetsAt.IsZero() || r.Failure == FailNone {
		var v any
		if json.Unmarshal([]byte(line), &v) == nil {
			codex.Walk(v, func(key string, val any) {
				switch strings.ToLower(strings.ReplaceAll(key, "_", "")) {
				case "resetsat", "resetat":
					if t, ok := codex.ParseTime(val); ok && r.ResetsAt.IsZero() {
						r.ResetsAt = t
					}
				case "codexerrorinfo":
					if s, ok := val.(string); ok && strings.Contains(strings.ToLower(s), "limit") {
						r.Failure, r.UsageLimited = FailUsageLimit, true
					}
				}
			})
		}
	}

	if kind != "" {
		r.EventTypes[kind]++
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
