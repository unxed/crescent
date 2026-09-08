//go:build windows

package runner

import "golang.org/x/sys/windows"

// processAlive reports whether a pid still refers to a running process.
// Unix signals do not exist here, so the handle is opened and its exit code
// asked for; STILL_ACTIVE means the process is running.
func processAlive(pid int) bool {
	const stillActive = 259
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return false
	}
	return code == stillActive
}
