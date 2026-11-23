//go:build windows

package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// psUTF8 makes PowerShell write UTF-8 (orchestra/tools/_shell_worker.py).
const psUTF8 = "$OutputEncoding = [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false); "

// powershellPath prefers the system copy by absolute path, so a powershell.exe
// in the project (the child's working directory) is never picked up.
func powershellPath() string {
	if dir, err := windows.GetSystemDirectory(); err == nil {
		p := filepath.Join(dir, "WindowsPowerShell", "v1.0", "powershell.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return p
	}
	return "powershell.exe"
}

func shellCommand(command string) *exec.Cmd {
	return exec.Command(powershellPath(), "-NoProfile", "-NonInteractive", "-Command", psUTF8+command)
}

// procTree is a Job Object with KILL_ON_JOB_CLOSE that owns the command and all
// its descendants, even ones whose parent has exited.
type procTree struct{ job windows.Handle }

// startTree starts cmd suspended, puts it in a new job and only then lets it run,
// so it cannot start a child outside the job. (Python reaches the same state
// with a bootstrap process that waits on stdin until assigned.)
func startTree(cmd *exec.Cmd) (*procTree, error) {
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
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	fail := func(err error) (*procTree, error) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		windows.CloseHandle(job)
		return nil, err
	}
	pid := uint32(cmd.Process.Pid)
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return fail(fmt.Errorf("OpenProcess: %w", err))
	}
	err = windows.AssignProcessToJobObject(job, process)
	windows.CloseHandle(process)
	if err != nil {
		return fail(fmt.Errorf("AssignProcessToJobObject: %w", err))
	}
	if err := resumeProcess(pid); err != nil {
		return fail(err)
	}
	return &procTree{job: job}, nil
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

func (t *procTree) kill() {
	if t.job != 0 {
		_ = windows.TerminateJobObject(t.job, 1)
	}
}

func (t *procTree) close() {
	if t.job != 0 {
		windows.CloseHandle(t.job)
		t.job = 0
	}
}
