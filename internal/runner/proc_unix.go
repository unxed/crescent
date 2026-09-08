//go:build !windows

package runner

import (
	"os"
	"syscall"
)

// processAlive reports whether a pid still refers to a running process.
// Signal 0 performs the permission and existence checks without delivering
// anything, which is exactly the question being asked.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
