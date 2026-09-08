//go:build windows

package codexcli

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var desktopAppNames = []string{"chatgpt", "codex-launcher"}

// AppRunning reports whether the ChatGPT desktop application is running.
// See the note in appcheck_unix.go for why it decides so much.
func AppRunning() (bool, string) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, ""
	}
	defer windows.CloseHandle(snap)

	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if windows.Process32First(snap, &e) != nil {
		return false, ""
	}
	for {
		name := strings.ToLower(windows.UTF16ToString(e.ExeFile[:]))
		for _, want := range desktopAppNames {
			if strings.Contains(name, want) {
				return true, name
			}
		}
		if windows.Process32Next(snap, &e) != nil {
			return false, ""
		}
	}
}
