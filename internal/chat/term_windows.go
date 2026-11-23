//go:build windows

package chat

import (
	"os"

	"golang.org/x/sys/windows"
)

// isTerminal reports whether fd is a console, the check isatty would do.
func isTerminal(fd uintptr) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(fd), &mode) == nil
}

// enableANSI turns on escape-sequence handling for a Windows console (Python's
// os.system("") trick). Errors are ignored: a console that refuses shows raw codes,
// which is what Python does too.
func enableANSI(f *os.File) {
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return
	}
	_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}
