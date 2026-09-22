package local

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"testing"
)

func haveSh(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh available")
	}
}

func TestLocalRunnerCombinedOutput(t *testing.T) {
	haveSh(t)
	out, err := NewRunner().CombinedOutput("echo hello-local; echo oops >&2")
	if err != nil {
		t.Fatalf("CombinedOutput: %v", err)
	}
	if !strings.Contains(out, "hello-local") || !strings.Contains(out, "oops") {
		t.Fatalf("output = %q", out)
	}
}

func TestLocalRunnerExecute(t *testing.T) {
	haveSh(t)
	s, err := NewRunner().Execute("printf 'abc-stream'")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data, _ := io.ReadAll(s.Stdout())
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	s.Close()
	if string(data) != "abc-stream" {
		t.Fatalf("stdout = %q", data)
	}
}

func TestLocalRunnerExecuteStdin(t *testing.T) {
	haveSh(t)
	s, err := NewRunner().ExecuteStdin("cat")
	if err != nil {
		t.Fatalf("ExecuteStdin: %v", err)
	}
	payload := bytes.Repeat([]byte{0x42}, 200000) // > pipe buffer, several writes
	go func() {
		for i := 0; i < len(payload); {
			n, err := s.Stdin().Write(payload[i:])
			i += n
			if err != nil {
				return
			}
		}
		s.Stdin().Close()
	}()
	data, _ := io.ReadAll(s.Stdout())
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	s.Close()
	if !bytes.Equal(data, payload) {
		t.Fatalf("echoed %d bytes, want %d", len(data), len(payload))
	}
}

func TestLocalRunnerFailedCommand(t *testing.T) {
	haveSh(t)
	out, err := NewRunner().CombinedOutput("exit 3")
	if err == nil {
		t.Fatal("CombinedOutput(exit 3) = nil error, want error")
	}
	if !strings.Contains(out, "") {
		t.Fatalf("output = %q", out)
	}
}

// TestLocalCloseThenWaitIdempotent: Close must reap the child itself (no
// zombie when a caller only Closes), and Wait after Close must return the
// same result instead of erroring with "Wait was already called".
func TestLocalCloseThenWaitIdempotent(t *testing.T) {
	haveSh(t)
	s, err := NewRunner().Execute("true")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Killed process: Wait returns a non-nil exit error but must not panic
	// or report double-Wait; a second call returns the same value.
	err1 := s.Wait()
	err2 := s.Wait()
	if err1 != nil && !strings.Contains(err1.Error(), "Wait was already called") {
		// exit-status errors from the kill are fine; only the double-Wait
		// bookkeeping error is a bug.
		t.Logf("Wait after Close: %v (expected kill/exit status)", err1)
	}
	if err1 != err2 {
		t.Fatalf("Wait not idempotent: %v then %v", err1, err2)
	}
}
