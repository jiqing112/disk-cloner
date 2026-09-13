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
