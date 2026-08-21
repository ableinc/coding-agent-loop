//go:build !unix

package proc

import (
	"os/exec"
	"time"
)

// Isolate is a no-op on platforms without process groups; the default
// exec.CommandContext behaviour (kill the direct child) applies.
func Isolate(cmd *exec.Cmd) {
	cmd.WaitDelay = 15 * time.Second
}
