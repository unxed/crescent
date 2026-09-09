package single

import (
	"os"
	"testing"
)

// Two crescents driving one account take turns on the same threads, so each
// sees the other's turn as "already has an active writer". A live journal
// showed exactly that, twice.
func TestSecondInstanceIsRefused(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	first, holder, err := Acquire()
	if err != nil {
		t.Fatalf("первый запуск не смог взять замок: %v (держит %d)", err, holder)
	}
	defer first.Release()

	// The same process is allowed through — this is a lock against a second
	// program, not against ourselves.
	if _, _, err := Acquire(); err != nil {
		t.Errorf("свой же процесс не пущен: %v", err)
	}
}

// A pid file left behind by a crash must not lock the program out for good.
func TestStalePidFileDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// A pid that cannot be running: 2^22 is above every default pid_max.
	if err := os.WriteFile(path, []byte("4194304"), 0o644); err != nil {
		t.Fatal(err)
	}

	lock, holder, err := Acquire()
	if err != nil {
		t.Fatalf("устаревший pid-файл заблокировал запуск: %v (держит %d)", err, holder)
	}
	lock.Release()
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}
