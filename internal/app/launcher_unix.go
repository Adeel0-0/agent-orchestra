//go:build !windows

package app

import (
	"os/exec"
	"runtime"
	"syscall"
)

func exclusiveAddrUse(_, _ string, _ syscall.RawConn) error { return nil }

// openBrowser hands the URL to the desktop's default browser without a shell.
func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
