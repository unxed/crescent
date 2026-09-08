//go:build !windows

package codexcli

import (
	"os"
	"path/filepath"
	"strings"
)

var desktopAppNames = []string{"chatgpt", "codex-launcher"}

// AppRunning reports whether the ChatGPT desktop application is running.
//
// It matters because a Codex session store has one owner. While the
// application runs it holds writers on the threads its goal engine manages,
// and a second client — us — is refused. Saying so before anything is
// attempted is kinder than a stream of unexplained refusals.
func AppRunning() (bool, string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(string(data)))
		for _, want := range desktopAppNames {
			if strings.Contains(name, want) {
				return true, name
			}
		}
	}
	return false, ""
}
