//go:build !windows

package cli

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// ReadInput reads a line from the terminal with full backspace/delete support.
// Uses raw terminal mode to handle control characters properly.
func ReadInput(prompt, def string) string {
	if def != "" {
		fmt.Printf("  %s [%s]: ", prompt, def)
	} else {
		fmt.Printf("  %s: ", prompt)
	}

	fd := int(os.Stdin.Fd())

	// If stdin is not a terminal (pipe/redirect), use simple read
	if !term.IsTerminal(fd) {
		return readLineSimple(def)
	}

	// Switch to raw mode for proper backspace handling
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return readLineSimple(def)
	}
	defer term.Restore(fd, oldState)

	var buf []byte
	oneByte := make([]byte, 4)

	for {
		n, err := os.Stdin.Read(oneByte)
		if err != nil || n == 0 {
			break
		}

		b := oneByte[0]

		switch {
		case b == '\r' || b == '\n':
			fmt.Print("\r\n")
			input := strings.TrimSpace(string(buf))
			if input == "" {
				return def
			}
			return input

		case b == 3:
			fmt.Print("\r\n")
			term.Restore(fd, oldState)
			os.Exit(0)

		case b == 127 || b == 8:
			if len(buf) > 0 {
				_, size := utf8.DecodeLastRune(buf)
				buf = buf[:len(buf)-size]
				fmt.Print("\b \b")
			}

		case b == 21:
			for len(buf) > 0 {
				_, size := utf8.DecodeLastRune(buf)
				buf = buf[:len(buf)-size]
				fmt.Print("\b \b")
			}

		case b == 23:
			for len(buf) > 0 {
				_, size := utf8.DecodeLastRune(buf)
				ch, _ := utf8.DecodeLastRune(buf)
				buf = buf[:len(buf)-size]
				fmt.Print("\b \b")
				if ch == ' ' || len(buf) == 0 {
					break
				}
			}

		case b >= 32:
			if n == 1 && b < 128 {
				buf = append(buf, b)
				fmt.Print(string(b))
			} else {
				char := oneByte[:n]
				buf = append(buf, char...)
				fmt.Print(string(char))
			}

		case b == 27:
			os.Stdin.Read(oneByte[:2])

		default:
		}
	}

	input := strings.TrimSpace(string(buf))
	if input == "" {
		return def
	}
	return input
}

// readLineSimple is a fallback for non-terminal stdin (pipes, redirects).
func readLineSimple(def string) string {
	var buf [4096]byte
	total := 0
	for {
		n, err := os.Stdin.Read(buf[total:])
		total += n
		if err != nil || total == 0 {
			return def
		}
		for i := total - n; i < total; i++ {
			if buf[i] == '\n' || buf[i] == '\r' {
				input := strings.TrimSpace(string(buf[:i]))
				if input == "" {
					return def
				}
				return input
			}
		}
		if total >= len(buf) {
			break
		}
	}
	input := strings.TrimSpace(string(buf[:total]))
	if input == "" {
		return def
	}
	return input
}

// ReadPassword reads a password without echoing to the terminal.
func ReadPassword(prompt string) string {
	fmt.Printf("  %s: ", prompt)
	pass, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(pass))
}
