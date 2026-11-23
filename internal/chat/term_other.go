//go:build !windows

package chat

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal reports whether fd is a terminal: only terminals answer TIOCGWINSZ.
func isTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ)
	return err == nil
}

// enableANSI is a no-op: Unix terminals handle escape sequences already.
func enableANSI(*os.File) {}
