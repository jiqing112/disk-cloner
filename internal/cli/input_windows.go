//go:build windows

package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// ReadInput reads a line from stdin. On Windows the console driver
// handles line editing (backspace, arrows) automatically.
func ReadInput(prompt, def string) string {
	if def != "" {
		fmt.Printf("  %s [%s]: ", prompt, def)
	} else {
		fmt.Printf("  %s: ", prompt)
	}

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return def
	}

	input := strings.TrimSpace(line)
	if input == "" {
		return def
	}
	return input
}

// ReadPassword reads a password from stdin. On Windows we don't hide the
// input (since term.ReadPassword can conflict with bufio.Reader on the
// same console handle, causing SSH auth to receive a garbled password).
// The password is printed to stdout ??not ideal but reliable.
func ReadPassword(prompt string) string {
	return ReadInput(prompt, "")
}
