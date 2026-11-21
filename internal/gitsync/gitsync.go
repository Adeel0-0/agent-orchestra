// Package gitsync runs the git steps of the handoff pipeline (orchestra/git_sync.py).
// Every call goes through one helper that records the last failure's stderr.
package gitsync

import (
	"strings"

	"github.com/Adeel0-0/agent-orchestra/internal/runner"
)

const maxErrorChars = 2000

// Status is the working tree state reported by status and the daemon.
type Status struct {
	Branch       string
	HeadSHA      string
	IsClean      bool
	ChangedFiles []string
}

// Engine runs git in Root.
type Engine struct {
	Root      string
	LastError string // stderr (or stdout) of the most recent failed git command, last 2000 chars
	runner    *runner.Runner
}

// New returns an engine for the repository at root.
func New(root string) *Engine {
	r := runner.New(root)
	return &Engine{Root: r.DefaultCwd, runner: r}
}

func (e *Engine) run(args ...string) runner.Result {
	return e.runner.Run(runner.ArgsCommand(append([]string{"git"}, args...)...), runner.Options{})
}

// git runs one step and records its error output on failure.
func (e *Engine) git(args ...string) bool {
	res := e.run(args...)
	if !res.Success {
		msg := res.Stderr
		if strings.TrimSpace(msg) == "" {
			msg = res.Stdout
		}
		msg = strings.TrimSpace(msg)
		if r := []rune(msg); len(r) > maxErrorChars {
			msg = string(r[len(r)-maxErrorChars:])
		}
		e.LastError = msg
	}
	return res.Success
}

// IsGitRepo reports whether Root is inside a work tree.
func (e *Engine) IsGitRepo() bool {
	res := e.run("rev-parse", "--is-inside-work-tree")
	return res.Success && strings.TrimSpace(res.Stdout) == "true"
}

// Status reads branch, short HEAD and changed files.
func (e *Engine) Status() Status {
	st := Status{Branch: "unknown", HeadSHA: "unknown"}
	if res := e.run("rev-parse", "--abbrev-ref", "HEAD"); res.Success {
		st.Branch = strings.TrimSpace(res.Stdout)
	}
	if res := e.run("rev-parse", "--short", "HEAD"); res.Success {
		st.HeadSHA = strings.TrimSpace(res.Stdout)
	}
	res := e.run("status", "--porcelain=v1", "-z")
	st.ChangedFiles = ParsePorcelainZ(res.Stdout)
	st.IsClean = len(st.ChangedFiles) == 0
	return st
}

// ParsePorcelainZ parses `git status --porcelain=v1 -z`: entries are "XY path\0",
// and a rename or copy is followed by its original path as a separate entry.
// Unlike the Python version, the entry is never trimmed before slicing, so the
// first character of the file name is kept when X is a space (" M file").
func ParsePorcelainZ(out string) []string {
	files := []string{}
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		files = append(files, entry[3:])
		if x := entry[0]; x == 'R' || x == 'C' {
			i++ // skip the original path
		}
	}
	return files
}

// StageAll runs `git add .`.
func (e *Engine) StageAll() bool { return e.git("add", ".") }

// Commit runs `git commit -m message`.
func (e *Engine) Commit(message string) bool { return e.git("commit", "-m", message) }

// Fetch runs `git fetch <remote> --prune`.
func (e *Engine) Fetch(remote string) bool { return e.git("fetch", remote, "--prune") }

// Rebase rebases onto upstream. On failure it runs `git rebase --abort` so the
// tree is left as it was, and keeps the original error in LastError.
func (e *Engine) Rebase(upstream string) bool {
	if e.git("rebase", upstream) {
		return true
	}
	original := e.LastError
	e.git("rebase", "--abort")
	e.LastError = original
	return false
}

// Push runs `git push <remote> <branch>`.
func (e *Engine) Push(remote, branch string) bool { return e.git("push", remote, branch) }

// FastForwardBranch moves remote target up to local source with
// `git push <remote> <source>:refs/heads/<target>`. The remote refuses anything
// but a fast-forward, and nothing is checked out under the agents.
func (e *Engine) FastForwardBranch(target, source, remote string) bool {
	return e.git("push", remote, source+":refs/heads/"+target)
}
