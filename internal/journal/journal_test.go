package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unxed/crescent/internal/appserver"
)

func TestPerGoalAndCombined(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	j, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	j.Label("t1", "Konsole")
	j.Record(appserver.Activity{ThreadID: "t1", Kind: "команда", Text: "go build"})
	j.Note("t1", "перезапущено")
	j.Note("", "наблюдение запущено") // top-level, no thread
	j.Close()

	dir, _ := Dir()
	konsole, err := os.ReadFile(filepath.Join(dir, "Konsole.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(konsole), "go build") || !strings.Contains(string(konsole), "перезапущено") {
		t.Errorf("журнал цели неполон:\n%s", konsole)
	}

	// A threadless note must not create its own file.
	if _, err := os.Stat(filepath.Join(dir, ".log")); err == nil {
		t.Error("создан файл для записи без треда")
	}

	all, _ := os.ReadFile(filepath.Join(dir, "all.log"))
	if !strings.Contains(string(all), "наблюдение запущено") || !strings.Contains(string(all), "Konsole") {
		t.Errorf("сводный журнал неполон:\n%s", all)
	}
}

// A label with slashes must not escape the journal directory.
func TestLabelSanitised(t *testing.T) {
	if got := safeName("f4/qt: [+]"); strings.ContainsAny(got, `/\:`) {
		t.Errorf("safeName оставил опасные символы: %q", got)
	}
}
