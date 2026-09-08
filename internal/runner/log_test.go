package runner

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// A daemon meant to run for days must not fill the disk with its own diary.
func TestLogRotatesOnceItGrows(t *testing.T) {
	setCacheDir(t, t.TempDir())

	path, err := LogPath()
	if err != nil {
		t.Fatal(err)
	}
	f, err := OpenLog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte("x"), maxLogBytes+1)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	f2, err := OpenLog()
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()

	if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
		t.Errorf("the log was not rolled over: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("the previous generation was not kept: %v", err)
	}
}

// Watching a run live and reading it afterwards must not be a choice.
func TestLogWriterFeedsBothConsoleAndFile(t *testing.T) {
	setCacheDir(t, t.TempDir())

	var console bytes.Buffer
	w, file := LogWriter(&console)
	if file == nil {
		t.Fatal("no log file was opened")
	}
	if _, err := w.Write([]byte("упёрлись в лимит\n")); err != nil {
		t.Fatal(err)
	}
	file.Close()

	if !strings.Contains(console.String(), "упёрлись") {
		t.Error("the terminal got nothing")
	}
	path, _ := LogPath()
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "упёрлись") {
		t.Errorf("the file got nothing: %v", err)
	}
}

// An unwritable cache directory must cost the log, not the run.
func TestLogWriterSurvivesAnUnusableCacheDir(t *testing.T) {
	blocker := t.TempDir() + "/file"
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	setCacheDir(t, blocker) // a file where a directory is expected

	var console bytes.Buffer
	w, file := LogWriter(&console)
	if file != nil {
		t.Error("a log file was opened where none could be")
	}
	if _, err := w.Write([]byte("работаем\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(console.String(), "работаем") {
		t.Error("output was lost when the file could not be opened")
	}
}
