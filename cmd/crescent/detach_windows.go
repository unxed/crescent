//go:build windows

package main

// detachFromConsole does nothing on Windows: the binary is linked with
// -H windowsgui, so it never owns a console in the first place.
func detachFromConsole(string) bool { return false }
