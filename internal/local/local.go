// Package local implements ssh.Runner against the LOCAL machine: commands
// run through /bin/sh exactly like the SSH variant runs them on a remote
// host. Used by mode 4 when the tool itself runs on the source machine in
// Alpine RAM OS — the local disk is dd'd and streamed to remote storage
// without any SSH connection to a source server.
package local

import (
	"fmt"
	"io"
	"os"
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

	// Hand-owned os.Pipes instead of StdoutPipe/StderrPipe/StdinPipe:
	// exec.Cmd.Wait closes the pipes it created, racing the caller's
	// stderr-draining goroutine (io.ReadAll in clone.go) and potentially
	// losing dd's final stats line — the local-mode truncation check
	// parses it. Pipes we create ourselves are not touched by Wait; Close
	// owns their lifetime instead.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("local: stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("local: stderr pipe: %w", err)
	}
	var stdinR, stdinW *os.File
	if withStdin {
		if stdinR, stdinW, err = os.Pipe(); err != nil {
			stdoutR.Close()
			stdoutW.Close()
			stderrR.Close()
			stderrW.Close()
			return nil, fmt.Errorf("local: stdin pipe: %w", err)
		}
		c.Stdin = stdinR
	}
	c.Stdout = stdoutW
	c.Stderr = stderrW
	if err := c.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		if stdinR != nil {
			stdinR.Close()
			stdinW.Close()
		}
		return nil, fmt.Errorf("local: start %q: %w", cmdStr, err)
	}
	// Drop this process's copies of the child-facing ends so EOF propagates
	// to the readers when the child exits.
	stdoutW.Close()
	stderrW.Close()
	if stdinR != nil {
		stdinR.Close()
	}
	return &proc{cmd: c, stdoutR: stdoutR, stderrR: stderrR, stdinW: stdinW}, nil
}

// proc is a running local command exposed through the ssh.Session
// interface, so the clone pipeline treats it like an SSH session.
type proc struct {
	cmd *exec.Cmd

	stdoutR *os.File
	stderrR *os.File
	stdinW  *os.File

	waitOnce sync.Once
	waitErr  error
}

func (p *proc) Stdout() io.Reader { return p.stdoutR }
func (p *proc) Stderr() io.Reader { return p.stderrR }

func (p *proc) Stdin() io.WriteCloser {
	// A nil *os.File would make a non-nil interface holding nil; callers
	// test the interface against nil.
	if p.stdinW == nil {
		return nil
	}
	return p.stdinW
}

// Wait blocks for the process and is idempotent — repeat calls return the
// same result, so Close-then-Wait orderings never error with
// "Wait was already called". It deliberately does not close the pipe read
// ends: a caller goroutine may still be draining stderr (see startProc).
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
	p.stdoutR.Close()
	p.stderrR.Close()
	if p.stdinW != nil {
		p.stdinW.Close()
	}
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
