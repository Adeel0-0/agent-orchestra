//go:build darwin

package openrouter

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// setNative stores the key as plain text the way Python keyring's macOS backend
// does. go-keyring would store "go-keyring-base64:..." which Python cannot read.
// The secret goes to `security -i` on stdin, never on a command line.
func setNative(service, account, value string) error {
	cmd := exec.Command("/usr/bin/security", "-i")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n",
		securityQuote(service), securityQuote(account), securityQuote(value))
	_, werr := io.WriteString(stdin, line)
	cerr := stdin.Close()
	if err := cmd.Wait(); err != nil {
		return err
	}
	if werr != nil {
		return werr
	}
	return cerr
}

// securityQuote single-quotes s for security(1)'s interactive parser.
func securityQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
