package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// A shell script named "codex" is not an executable on Windows, so the tests
// that spawn one failed there with "executable file not found". Rather than
// skip them on the platform that most needs covering, the test binary doubles
// as the mock: it re-executes itself, and the environment tells it what to
// print. That works identically everywhere and costs nothing to build.
const (
	envMockFile = "CRESCENT_TEST_MOCK_FILE"
	envMockExit = "CRESCENT_TEST_MOCK_EXIT"
)

func TestMain(m *testing.M) {
	// The stream travels in a file, not in the environment: one of the tests
	// feeds a 300 KB event on purpose, and that is far past the size a process
	// environment will carry.
	if path, ok := os.LookupEnv(envMockFile); ok {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(70)
		}
		os.Stdout.Write(data)
		code, _ := strconv.Atoi(os.Getenv(envMockExit))
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// mockCodex returns a policy whose CodexPath is this very test binary, primed
// to print the given stream and exit with the given code.
func mockCodex(t *testing.T, stdout string, exit int) Policy {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// An empty stream must mean no output at all: a lone newline would be one
	// unparsable line, which is a different diagnosis entirely.
	body := stdout
	if body != "" {
		body += "\n"
	}
	path := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envMockFile, path)
	t.Setenv(envMockExit, strconv.Itoa(exit))

	p := DefaultPolicy()
	p.CodexPath = self
	return p
}

// jsonPath renders a filesystem path for embedding in a JSON string. On Windows
// a raw path carries backslashes, which are escapes in JSON: a fixture built by
// concatenation produced invalid records there, so no goal was found and half
// the loop tests failed for a reason that had nothing to do with the loop.
func jsonPath(p string) string {
	q := strconv.Quote(filepath.ToSlash(p))
	return q[1 : len(q)-1]
}
