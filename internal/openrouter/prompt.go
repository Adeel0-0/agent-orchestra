package openrouter

import (
	"errors"
	"fmt"
	"io"
	"os"
)

var (
	// ErrNotInteractive: stdin is not a terminal that can hide input (exit 2).
	ErrNotInteractive = errors.New("Run orchestra openrouter-key in an interactive terminal; the key is entered hidden.")
	// ErrCancelled: Ctrl+C or end of input at the prompt (exit 130).
	ErrCancelled = errors.New("Key setup cancelled.")
)

// keyPrompt is the hidden-input prompt text.
const keyPrompt = "OpenRouter API key (hidden): "

// readHidden reads one line from a terminal with echo off. Replaced in tests.
var readHidden = readHiddenLine

// SaveKeyInteractive implements `orchestra openrouter-key`: it prompts on out,
// reads the key from the stdin terminal without echo and saves it in the OS
// keyring, printing a confirmation. It returns ErrNotInteractive (exit 2),
// ErrCancelled (exit 130) or a *CredentialError (exit 2); the key is never
// printed or returned.
func SaveKeyInteractive(stdin *os.File, out io.Writer) error {
	value, err := readHidden(stdin, out)
	if err != nil {
		return err
	}
	if err := SetKey(value); err != nil {
		return err
	}
	fmt.Fprintln(out, "OpenRouter key saved in the OS keyring.")
	return nil
}
