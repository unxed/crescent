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

// Cutting a label's tail threw away the only thing that told two goals apart:
// "Следовать инструкции Лунобот-1" and "…-2" differ in the last character.
func TestLabelShortenedFromTheMiddle(t *testing.T) {
	a := middle("Следовать инструкции Лунобот-1", 24)
	b := middle("Следовать инструкции Лунобот-2", 24)
	if a == b {
		t.Fatalf("две разные цели выглядят одинаково: %q", a)
	}
	if !strings.HasSuffix(a, "1") || !strings.HasSuffix(b, "2") {
		t.Errorf("номер потерян: %q / %q", a, b)
	}
}

// "What is happening now" should not require scrolling to the bottom.
func TestNewestLineComesFirst(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	j, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	j.Label("t1", "Konsole")
	j.Record(appserver.Activity{ThreadID: "t1", Kind: "ход", Text: "первое"})
	j.Record(appserver.Activity{ThreadID: "t1", Kind: "ход", Text: "второе"})
	j.Close()

	tail := j.TailOf("Konsole", 50)
	first := strings.SplitN(tail, "\n", 2)[0]
	if !strings.Contains(first, "второе") {
		t.Errorf("сверху не самое новое:\n%s", tail)
	}
}

// A command line pasted into the log brought its own indentation with it.
func TestWhitespaceCollapsed(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	j, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	j.Label("t1", "Konsole")
	j.Record(appserver.Activity{ThreadID: "t1", Kind: "команда", Text: "go   build    ./...\n\n  тест"})
	j.Close()

	if got := j.TailOf("Konsole", 10); !strings.Contains(got, "go build ./... тест") {
		t.Errorf("пробелы не схлопнуты: %q", got)
	}
}

// tracing colours its output when it thinks it has a terminal; in a text view
// those escapes rendered as a wall of unreadable boxes.
func TestServerLinesStrippedAndSeparated(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	j, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	j.Server("\x1b[2m2026-09-09T01:59:34Z\x1b[0m \x1b[32mINFO\x1b[0m app_server.request")
	j.Server("2026-09-09T02:00:00Z ERROR codex_app_server: всё плохо")
	j.Close()

	dir, _ := Dir()
	srv, err := os.ReadFile(filepath.Join(dir, "app-server.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(srv), "\x1b") {
		t.Error("ANSI-последовательности не вырезаны")
	}
	if !strings.Contains(string(srv), "app_server.request") {
		t.Error("строка сервера не попала в свой файл")
	}

	// The human journal gets the error but not the INFO noise: on a live run
	// that ratio was 15768 machine lines to 96 of ours.
	all, _ := os.ReadFile(filepath.Join(dir, "all.log"))
	if strings.Contains(string(all), "app_server.request") {
		t.Error("INFO-шум сервера попал в человеческий журнал")
	}
	if !strings.Contains(string(all), "всё плохо") {
		t.Error("ошибка сервера не доведена до человека")
	}
}

// A quiet goal's log showed a day-old failure at the top, and newest-first
// order made it look like it had just happened.
func TestQuietGoalSaysNothingHappened(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	j, _ := Open()
	j.Label("t1", "Konsole")
	j.Record(appserver.Activity{ThreadID: "t1", Kind: "crescent", Text: "старая ошибка"})
	j.Close()

	// A second run that never touches this goal.
	j2, _ := Open()
	defer j2.Close()
	j2.Label("t1", "Konsole") // the window registers names at startup
	got := j2.TailOf("Konsole", 50)
	if !strings.Contains(got, "ничего не происходило") {
		t.Errorf("молчащая цель не помечена:\n%s", got)
	}
	if !strings.Contains(got, "старая ошибка") {
		t.Error("прошлые записи потеряны")
	}

	// Once something does happen, the notice goes away.
	j2.Record(appserver.Activity{ThreadID: "t1", Kind: "ход", Text: "начат"})
	if strings.Contains(j2.TailOf("Konsole", 50), "ничего не происходило") {
		t.Error("пометка осталась после появления событий")
	}
}

// Without a date, a line from yesterday looks like a line from a minute ago —
// which is precisely how a day-old failure was read as a live one.
func TestEveryLineCarriesADate(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	j, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	j.Label("t1", "Konsole")
	j.Record(appserver.Activity{ThreadID: "t1", Kind: "ход", Text: "начат"})
	j.Note("", "наблюдение запущено")
	j.Close()

	dir, _ := Dir()
	for _, name := range []string{"all.log", "Konsole.log"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if line == "" {
				continue
			}
			// "MM-DD HH:MM:SS" — the date is the first token.
			if len(line) < 14 || line[2] != '-' || line[5] != ' ' {
				t.Errorf("%s: строка без даты: %q", name, line)
				break
			}
		}
	}
}
