//go:build !windows && !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package openrouter

import (
	"io"
	"os"
)

func readHiddenLine(*os.File, io.Writer) (string, error) { return "", ErrNotInteractive }
