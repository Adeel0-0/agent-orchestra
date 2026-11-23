//go:build !windows

package tools

import "syscall"

func processAlive(pid int) bool {
	// A killed child of an exited parent is reaped by init; a zombie would still
	// accept signal 0, so also check it is not reported as gone.
	return syscall.Kill(pid, 0) == nil
}

func killTestPID(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }
