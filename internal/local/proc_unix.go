//go:build !windows

package local

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the command in its own process group so later signals
// reach the whole tree — the sh wrapper plus any background children it
// spawned (zero-fill's dd).
func setProcGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcGroup signals the process group led by c (negative pid = group
// semantics). A bare pid would only hit the sh wrapper and orphan its
// background children.
func killProcGroup(c *exec.Cmd, sig syscall.Signal) error {
	if c.Process == nil {
		return nil
	}
	return syscall.Kill(-c.Process.Pid, sig)
}
