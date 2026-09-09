// Package single keeps one crescent running at a time.
package single

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Lock is a held single-instance lock.
type Lock struct{ path string }

// Path is the pid file the lock lives in.
func Path() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "crescent", "crescent.pid"), nil
}

// Acquire takes the lock, or reports which process already holds it.
//
// Two crescents driving the same account is not a harmless duplicate: they take
// turns on the same threads, so each sees the other's turn as "already has an
// active writer" and both log restarts that never happened. A live journal
// showed exactly that — two "наблюдение запущено" a second apart, then a stream
// of writer conflicts.
func Acquire() (*Lock, int, error) {
	path, err := Path()
	if err != nil {
		return nil, 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, 0, err
	}

	if data, err := os.ReadFile(path); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid != os.Getpid() {
			if alive(pid) {
				return nil, pid, fmt.Errorf("crescent уже запущен (pid %d)", pid)
			}
		}
	}
	// A stale file is just a file: the process it named is gone.
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, 0, err
	}
	return &Lock{path: path}, 0, nil
}

// Release drops the lock, but only if it is still ours: a restart that already
// reclaimed it must not be unlocked by the previous process shutting down.
func (l *Lock) Release() {
	if l == nil {
		return
	}
	if data, err := os.ReadFile(l.path); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid != os.Getpid() {
			return
		}
	}
	_ = os.Remove(l.path)
}
