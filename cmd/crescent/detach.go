//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// detachFromConsole re-launches crescent detached from the terminal and returns
// true if the caller should now exit.
//
// Launching the window from a shell should give the shell back, the way any
// desktop application does. On Windows the linker flag -H windowsgui already
// means there is no console to hold; on Unix nothing does that for us, so the
// process re-executes itself in a new session with its standard streams pointed
// at the log, and the original returns to the prompt.
func detachFromConsole(logPath string) bool {
	if os.Getenv(envDetached) != "" {
		return false // already the detached child
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}

	// Truncated, not appended: this file catches whatever the detached process
	// prints before the journal takes over, and appending to it forever is how
	// it reached gigabytes. One run's worth is all it is for.
	out, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		out = nil
	}

	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), envDetached+"=1")
	cmd.Stdin = nil
	if out != nil {
		cmd.Stdout, cmd.Stderr = out, out
	}
	// Setsid detaches from the controlling terminal, so closing the shell — or
	// the shell exiting — does not take the window with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return false
	}
	if out != nil {
		_ = out.Close()
	}
	return true
}
