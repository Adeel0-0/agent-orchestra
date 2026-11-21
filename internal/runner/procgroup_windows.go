//go:build windows

package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// buildCommand runs shell strings exactly as Python's subprocess(shell=True)
// does on Windows: `<COMSPEC> /c "<command>"`, passed as the raw command line so
// Go's argument quoting (which cmd.exe does not understand) is not applied.
func buildCommand(ctx context.Context, c Command) (*exec.Cmd, error) {
	attr := &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	if c.Args != nil {
		if len(c.Args) == 0 {
			return nil, fmt.Errorf("empty command")
		}
		cmd := exec.CommandContext(ctx, c.Args[0], c.Args[1:]...)
		cmd.SysProcAttr = attr
		return cmd, nil
	}
	comspec := os.Getenv("COMSPEC")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	cmd := exec.CommandContext(ctx, comspec)
	attr.CmdLine = fmt.Sprintf(`%s /c "%s"`, comspec, c.Shell)
	cmd.SysProcAttr = attr
	return cmd, nil
}

// processGroup is a Job Object with KILL_ON_JOB_CLOSE. Children created after the
// process joins the job are in it too, so terminating the job ends the whole tree.
type processGroup struct {
	job      windows.Handle
	pid      int
	assigned bool
}

func newProcessGroup(*exec.Cmd) (*processGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return &processGroup{job: job}, nil
}

// started puts the new process in the job. It runs right after CreateProcess
// returns, before cmd.exe has had time to start its own children. If assignment
// fails (for example a parent job forbids it), kill falls back to taskkill /T.
func (g *processGroup) started(pid int) {
	g.pid = pid
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	g.assigned = windows.AssignProcessToJobObject(g.job, h) == nil
}

func (g *processGroup) kill() error {
	if g.assigned {
		if err := windows.TerminateJobObject(g.job, 1); err == nil {
			return nil
		}
	}
	if g.pid == 0 {
		return nil
	}
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(g.pid)).Run()
}

// close releases the job handle; KILL_ON_JOB_CLOSE ends anything still in it.
func (g *processGroup) close() {
	if g.job != 0 {
		windows.CloseHandle(g.job)
		g.job = 0
	}
}
