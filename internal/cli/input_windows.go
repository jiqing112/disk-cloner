//go:build windows

package cli

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// lastWasCR remembers that the previous line ended with '\r' so the '\n' of
// the CRLF pair is skipped at the start of the next read instead of being
// treated as an empty extra line.
var lastWasCR bool

// readLine reads one line from stdin byte-by-byte. A bufio.Reader shared
// across ReadInput calls would swallow pasted multi-line input into its
// internal buffer and lose it between calls; the console driver's own line
// buffer survives between os.Stdin reads, so byte-wise reads are paste-safe.
func readLine() (string, bool) {
	var buf []byte
	one := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(one)
		if err != nil || n == 0 {
			return string(buf), false
		}
		b := one[0]
		if b == '\n' {
			if lastWasCR {
				// LF of a CRLF pair — the CR already ended the line.
				lastWasCR = false
				continue
			}
			return string(buf), true
		}
		if b == '\r' {
			lastWasCR = true
			return string(buf), true
		}
		lastWasCR = false
		buf = append(buf, b)
	}
}

// ReadInput reads a line from stdin. On Windows the console driver handles
// line editing (backspace, arrows) automatically.
func ReadInput(prompt, def string) string {
	if def != "" {
		fmt.Printf("  %s [%s]: ", prompt, def)
	} else {
		fmt.Printf("  %s: ", prompt)
	}

	line, _ := readLine()
	input := strings.TrimSpace(line)
	if input == "" {
		return def
	}
	return input
}

// ReadPassword reads a password without echoing it. term.ReadPassword talks
// to the console directly, which is safe now that regular input is read
// byte-by-byte (no bufio buffer holding already-typed characters).
func ReadPassword(prompt string) string {
	fmt.Printf("  %s: ", prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		pass, err := term.ReadPassword(fd)
		fmt.Println()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(pass))
	}
	// Not a console (piped input) — fall back to a plain line read.
	line, _ := readLine()
	return strings.TrimSpace(line)
}

// ReadInputPath reads a file path. The Windows console driver handles line
// editing; Tab completion is not provided (dragging a file into the console
// window is the usual way to input paths).
func ReadInputPath(prompt, def string) string {
	return ReadInput(prompt, def)
}
