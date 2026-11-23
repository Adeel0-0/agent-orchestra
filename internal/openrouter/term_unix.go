//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package openrouter

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"golang.org/x/sys/unix"
)

// readHiddenLine turns terminal echo off (keeping line editing), reads one line
// and restores the terminal, also when Ctrl+C is pressed, like Python's getpass.
func readHiddenLine(stdin *os.File, out io.Writer) (string, error) {
	fd := int(stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return "", ErrNotInteractive
	}
	hidden := *old
	hidden.Lflag &^= unix.ECHO
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &hidden); err != nil {
		return "", ErrNotInteractive
	}
	defer unix.IoctlSetTermios(fd, ioctlSetTermios, old)
	fmt.Fprint(out, keyPrompt)
	defer fmt.Fprint(out, "\n")

	type result struct {
		line string
		err  error
	}
	lines := make(chan result, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 1)
		for {
			n, err := stdin.Read(buf)
			if n == 1 {
				if buf[0] == '\n' {
					lines <- result{line: b.String()}
					return
				}
				b.WriteByte(buf[0])
			}
			if err != nil {
				if b.Len() == 0 {
					lines <- result{err: ErrCancelled}
				} else {
					lines <- result{line: b.String()}
				}
				return
			}
		}
	}()
	select {
	case r := <-lines:
		return r.line, r.err
	case <-interrupts:
		return "", ErrCancelled
	}
}
