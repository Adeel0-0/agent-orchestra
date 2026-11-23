//go:build windows

package openrouter

import (
	"fmt"
	"io"
	"os"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// readHiddenLine reads a console line one character at a time with echo, line
// editing and Ctrl+C processing off, like Python's getpass on Windows: Enter
// ends, Backspace deletes, Ctrl+C cancels. A redirected stdin (pipe, file, or a
// mintty pty) is refused rather than read with echo.
func readHiddenLine(stdin *os.File, out io.Writer) (string, error) {
	h := windows.Handle(stdin.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return "", ErrNotInteractive
	}
	raw := mode &^ (windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT)
	if err := windows.SetConsoleMode(h, raw); err != nil {
		return "", ErrNotInteractive
	}
	defer windows.SetConsoleMode(h, mode)
	fmt.Fprint(out, keyPrompt)
	defer fmt.Fprint(out, "\r\n")
	var units []uint16
	for {
		var buf [1]uint16
		var n uint32
		if err := windows.ReadConsole(h, &buf[0], 1, &n, nil); err != nil {
			return "", ErrCancelled
		}
		if n == 0 {
			continue
		}
		switch c := buf[0]; c {
		case '\r', '\n':
			return string(utf16.Decode(units)), nil
		case 3: // Ctrl+C
			return "", ErrCancelled
		case '\b':
			if len(units) > 0 {
				units = units[:len(units)-1]
				if len(units) > 0 && units[len(units)-1] >= 0xD800 && units[len(units)-1] <= 0xDBFF {
					units = units[:len(units)-1]
				}
			}
		default:
			units = append(units, c)
		}
	}
}
