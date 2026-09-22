// Package local implements ssh.Runner against the LOCAL machine: commands
// run through /bin/sh exactly like the SSH variant runs them on a remote
// host. Used by mode 4 when the tool itself runs on the source machine in
// Alpine RAM OS — the local disk is dd'd and streamed to remote storage
// without any SSH connection to a source server.
package local

import (
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/crypto/ssh"

	sshclient "disk-cloner/internal/ssh"
)

// NewRunner returns a ssh.Runner that executes commands locally.
func NewRunner() sshclient.Runner {
	return runner{}
}

type runner struct{}

func (runner) CombinedOutput(cmd string) (string, error) {
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	return string(out), err
}

func (runner) Execute(cmd string) (sshclient.Session, error) {
	return startProc(cmd, false)
}

func (runner) ExecuteStdin(cmd string) (sshclient.Session, error) {
	return startProc(cmd, true)
}

func (runner) IsConnected() bool { return true }

func startProc(cmdStr string, withStdin bool) (sshclient.Session, error) {
	c := exec.Command("sh", "-c", cmdStr)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("local: stdout pipe: %w", err)
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("local: stderr pipe: %w", err)
	}
	var stdin io.WriteCloser
	if withStdin {
		if stdin, err = c.StdinPipe(); err != nil {
			return nil, fmt.Errorf("local: stdin pipe: %w", err)
		}
	}
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("local: start %q: %w", cmdStr, err)
	}
	return &proc{cmd: c, stdout: stdout, stderr: stderr, stdin: stdin}, nil
}

// proc is a running local command exposed through the ssh.Session
// interface, so the clone pipeline treats it like an SSH session.
type proc struct {
	cmd    *exec.Cmd
	stdout io.Reader
	stderr io.Reader
	stdin  io.WriteCloser

	waitOnce sync.Once
	waitErr  error
}

func (p *proc) Stdout() io.Reader     { return p.stdout }
func (p *proc) Stderr() io.Reader     { return p.stderr }
func (p *proc) Stdin() io.WriteCloser { return p.stdin }

// Wait blocks for the process and is idempotent — repeat calls return the
// same result, so Close-then-Wait orderings never error with
// "Wait was already called".
func (p *proc) Wait() error {
	p.waitOnce.Do(func() { p.waitErr = p.cmd.Wait() })
	return p.waitErr
}

// Close tears the process down (used on abort/error paths) AND reaps it,
// so a caller that only Close never leaves a zombie behind. After a
// completed Wait the kill errors harmlessly.
func (p *proc) Close() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	p.Wait()
	return nil
}

func (p *proc) Signal(sig ssh.Signal) error {
	if p.cmd.Process == nil {
		return nil
	}
	switch sig {
	case ssh.SIGTERM:
		return p.cmd.Process.Signal(syscall.SIGTERM)
	case ssh.SIGKILL:
		return p.cmd.Process.Kill()
	}
	return fmt.Errorf("local: unsupported signal %q", sig)
}
