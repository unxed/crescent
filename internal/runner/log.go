package runner

import (
	"io"
	"os"
	"path/filepath"

	"github.com/unxed/crescent/internal/codex"
)

// maxLogBytes is when the log is rolled over. A daemon meant to run for days
// writes a few lines per turn, so this is months of history — but "a few lines
// per turn" stops being true the moment something goes wrong in a loop, and an
// unbounded log on a laptop is its own kind of failure.
const maxLogBytes = 4 << 20

// LogPath is the file both modes write to.
//
// The tray has no console, so without a file its output would vanish; and the
// terminal mode writes here too, because "what was it doing at three in the
// morning" is a question asked long after the terminal is gone.
func LogPath() (string, error) {
	dir, err := codex.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "crescent.log"), nil
}

// OpenLog returns the log file, rolling the previous one aside if it has grown
// past the limit. One generation is kept: enough to look back through the last
// session, not enough to accumulate.
func OpenLog() (*os.File, error) {
	path, err := LogPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogBytes {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// LogWriter sends output to the terminal and to the file at once, so a run
// watched live is also a run that can be read afterwards. A file that cannot be
// opened is not fatal: the terminal still gets everything.
func LogWriter(console io.Writer) (io.Writer, *os.File) {
	f, err := OpenLog()
	if err != nil {
		return console, nil
	}
	if console == nil {
		return f, f
	}
	return io.MultiWriter(console, f), f
}
