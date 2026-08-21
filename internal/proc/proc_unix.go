//go:build unix

// Package proc holds the process-control details shared by every external
// command this daemon runs.
package proc

import (
	"os/exec"
	"syscall"
	"time"
)

// Isolate puts cmd in its own process group and makes context cancellation
// kill that whole group.
//
// Without this, cancelling only kills the direct child. Both commands this
// daemon runs — the Claude CLI and a repository's test suite — routinely spawn
// children of their own, and those survivors keep the stdout pipe open, so a
// command that hit its timeout would leave the worker blocked on a read long
// after it was supposed to be dead.
func Isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// A negative pid signals the entire process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Do not wait forever for a stray descendant to close the pipe.
	cmd.WaitDelay = 15 * time.Second
}
