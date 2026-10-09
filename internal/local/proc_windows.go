//go:build windows

package local

import (
	"os/exec"
	"syscall"
)

// Mode 4 never runs on Windows (main.go gates -l and the interactive entry
// to Linux), so these fallbacks degrade to single-process semantics.

func setProcGroup(c *exec.Cmd) {}

func killProcGroup(c *exec.Cmd, sig syscall.Signal) error {
	if c.Process == nil {
		return nil
	}
	if sig == syscall.SIGKILL {
		return c.Process.Kill()
	}
	return c.Process.Signal(sig)
}
