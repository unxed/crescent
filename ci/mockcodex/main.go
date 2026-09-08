// Command mockcodex impersonates the Codex CLI well enough to rehearse
// crescent without an account.
//
// The events it emits follow codex-rs/exec/src/exec_events.rs and the
// usage-limit wording follows codex-rs/protocol/src/error.rs, both from the
// public Codex repository. That is the whole point: a mock invented from
// guesswork would only prove that crescent still understands the guess.
//
// It is a Go program rather than a shell script because Windows cannot spawn a
// .cmd directly from a process — and Windows is precisely the platform this
// rehearsal exists to cover.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println("codex-cli 0.153.4-mock")
		return
	}
	if len(args) > 1 && args[0] == "exec" && args[1] == "--help" {
		fmt.Println("--json")
		fmt.Println("Commands:")
		fmt.Println("  resume")
		return
	}

	// Turn counter in a file, so behaviour changes across separate invocations
	// the way a real quota does.
	counter := os.Getenv("MOCK_COUNTER")
	n := 0
	if b, err := os.ReadFile(counter); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	n++
	if counter != "" {
		_ = os.WriteFile(counter, []byte(strconv.Itoa(n)), 0o644)
	}

	out := os.Stdout
	fmt.Fprintln(out, `{"type":"thread.started","thread_id":"01a04f59-3ac3-7790-9f25-a0bd6c10ca2b"}`)
	fmt.Fprintln(out, `{"type":"turn.started"}`)

	limitAfter := 2
	if v := os.Getenv("MOCK_LIMIT_AFTER"); v != "" {
		if k, err := strconv.Atoi(v); err == nil {
			limitAfter = k
		}
	}
	if n > limitAfter {
		fmt.Fprintln(out, `{"type":"turn.failed","error":{"message":"You've hit your usage limit. Try again later."}}`)
		os.Exit(1)
	}

	fmt.Fprintln(out, `{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"Ход выполнен."}}`)
	fmt.Fprintln(out, `{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":0,`+
		`"cache_write_input_tokens":0,"output_tokens":340,"reasoning_output_tokens":90}}`)
}
