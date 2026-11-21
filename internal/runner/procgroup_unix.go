//go:build !windows

package runner

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
)

func buildCommand(ctx context.Context, c Command) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	if c.Args != nil {
		if len(c.Args) == 0 {
			return nil, fmt.Errorf("empty command")
		}
		cmd = exec.CommandContext(ctx, c.Args[0], c.Args[1:]...)
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", c.Shell)
	}
	// Own process group: the pgid is the child's pid, so kill(-pgid) ends the tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

type processGroup struct{ pid int }

func newProcessGroup(*exec.Cmd) (*processGroup, error) { return &processGroup{}, nil }

func (g *processGroup) started(pid int) { g.pid = pid }

func (g *processGroup) kill() error {
	if g.pid == 0 {
		return nil
	}
	err := syscall.Kill(-g.pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

func (g *processGroup) close() {}
