//go:build windows

package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// buildCommand runs shell strings exactly as Python's subprocess(shell=True)
// does on Windows: `<COMSPEC> /c "<command>"`, passed as the raw command line so
// Go's argument quoting (which cmd.exe does not understand) is not applied.
//
// Every process starts suspended; started() puts it in the Job Object and only then
// resumes it, so nothing it spawns can run outside the job.
func buildCommand(ctx context.Context, c Command) (*exec.Cmd, error) {
	attr := &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED}
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
	closed   bool
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

// started puts the suspended process in the job, then resumes it. If assignment
// fails (for example a parent job forbids it), the process still runs and kill
// falls back to taskkill /T. If it cannot be resumed it is terminated.
func (g *processGroup) started(pid int) error {
	g.pid = pid
	if h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid)); err == nil {
		g.assigned = windows.AssignProcessToJobObject(g.job, h) == nil
		windows.CloseHandle(h)
	}
	if err := resumeProcess(uint32(pid)); err != nil {
		g.kill()
		return err
	}
	return nil
}

// resumeProcess resumes the threads of a process created suspended (it has one).
func resumeProcess(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := 0
	for err = windows.Thread32First(snap, &entry); err == nil; err = windows.Thread32Next(snap, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return fmt.Errorf("OpenThread: %w", err)
		}
		_, err = windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		if err != nil {
			return fmt.Errorf("ResumeThread: %w", err)
		}
		resumed++
	}
	if resumed == 0 {
		return fmt.Errorf("could not find the command's main thread")
	}
	return nil
}

// kill ends the tree while the process may still be running. After close the pid
// may already belong to an unrelated process, so nothing is killed by pid then.
func (g *processGroup) kill() error {
	if g.closed {
		return nil
	}
	if g.assigned {
		if err := windows.TerminateJobObject(g.job, 1); err == nil {
			return nil
		}
	}
	if g.pid == 0 {
		return nil
	}
	taskkill := "taskkill.exe"
	if dir, err := windows.GetSystemDirectory(); err == nil {
		taskkill = filepath.Join(dir, "taskkill.exe") // never a taskkill.exe found on PATH
	}
	return exec.Command(taskkill, "/T", "/F", "/PID", strconv.Itoa(g.pid)).Run()
}

// killLeftovers ends processes the command left behind after it exited. On Windows
// closing the job (KILL_ON_JOB_CLOSE) already does that.
func (g *processGroup) killLeftovers() {}

// close releases the job handle; KILL_ON_JOB_CLOSE ends anything still in it.
func (g *processGroup) close() {
	if g.job != 0 {
		windows.CloseHandle(g.job)
		g.job = 0
	}
	g.closed = true
}
