//go:build !windows

package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// ReadInput reads a line from the terminal with full backspace/delete support.
// Uses raw terminal mode to handle control characters properly.
// If def is not empty, it is pre-filled into the input buffer so the user
// can edit it with backspace/arrows rather than retyping entirely.
func ReadInput(prompt, def string) string {
	fmt.Printf("  %s: ", prompt)

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
	// Pre-fill buffer with default so the user can edit it
	if def != "" {
		buf = []byte(def)
		fmt.Print(def)
	}
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
			os.Exit(130)

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
// Data that arrives without a trailing newline (EOF) is still returned.
func readLineSimple(def string) string {
	var buf [4096]byte
	total := 0
	for {
		n, err := os.Stdin.Read(buf[total:])
		total += n
		for i := 0; i < total; i++ {
			if buf[i] == '\n' || buf[i] == '\r' {
				input := strings.TrimSpace(string(buf[:i]))
				if input == "" {
					return def
				}
				return input
			}
		}
		if err != nil {
			// EOF (or error) before a newline: use what we got.
			input := strings.TrimSpace(string(buf[:total]))
			if input == "" {
				return def
			}
			return input
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

// ReadInputPath reads a file path from the terminal with shell-like Tab
// completion. Backspace/Ctrl+U/Ctrl+W edit the line, Tab completes file
// and directory names, a leading ~ is expanded to the home directory.
// Falls back to the plain reader when stdin is not a terminal.
func ReadInputPath(prompt, def string) string {
	fmt.Printf("  %s: ", prompt)

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return readLineSimple(def)
	}

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
			return expandHomePath(strings.TrimSpace(string(buf)), def)

		case b == 3:
			fmt.Print("\r\n")
			term.Restore(fd, oldState)
			os.Exit(130)

		case b == '\t':
			completeTab(&buf, prompt, oneByte)

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

	return expandHomePath(strings.TrimSpace(string(buf)), def)
}

// expandHomePath expands a leading ~ to the home directory.
// Empty input returns def.
func expandHomePath(input, def string) string {
	if input == "" {
		return def
	}
	if input == "~" || strings.HasPrefix(input, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if input == "~" {
				return home
			}
			return home + "/" + strings.TrimPrefix(input, "~/")
		}
	}
	return input
}

// pathCompletion is the result of one Tab press on a path input line.
type pathCompletion struct {
	add  string   // text to append to the current buffer
	list []string // candidates to display when ambiguous
}

// completeTab performs one Tab completion step on buf.
func completeTab(buf *[]byte, prompt string, oneByte []byte) {
	comp := completePath(string(*buf))

	if comp.add != "" {
		*buf = append(*buf, comp.add...)
		fmt.Print(comp.add)
		return
	}

	if len(comp.list) == 0 {
		fmt.Print("\a") // bell: no match
		return
	}

	fmt.Print("\r\n")
	if len(comp.list) > 50 {
		fmt.Printf("  共 %d 个候选, 按 y 显示全部, 其他键跳过: ", len(comp.list))
		n, _ := os.Stdin.Read(oneByte)
		fmt.Print("\r\n")
		if n > 0 && (oneByte[0] == 'y' || oneByte[0] == 'Y') {
			printMatches(comp.list)
		}
	} else {
		printMatches(comp.list)
	}
	fmt.Printf("  %s: %s", prompt, string(*buf))
}

// completePath completes the last path component of line, mimicking
// bash behavior: single match is inserted (directories get a trailing
// slash), multiple matches extend to the longest common prefix and are
// listed on the next Tab.
func completePath(line string) pathCompletion {
	if line == "" {
		return pathCompletion{}
	}

	var dirPart, prefix string
	if slash := strings.LastIndexByte(line, '/'); slash >= 0 {
		dirPart = line[:slash+1]
		prefix = line[slash+1:]
	} else {
		prefix = line
	}

	// Expand ~ (and resolve relative paths) for directory lookup only;
	// the buffer keeps exactly what the user typed.
	lookup := dirPart
	if lookup == "~/" || lookup == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			lookup = home + "/"
		}
	}
	if lookup == "" {
		lookup = "./"
	}

	entries, err := os.ReadDir(lookup)
	if err != nil {
		return pathCompletion{}
	}

	// Hidden entries only match when the prefix itself starts with '.'
	var names []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return pathCompletion{}
	}
	sort.Strings(names)

	if len(names) == 1 {
		name := names[0]
		add := name[len(prefix):]
		if e, ok := entryIsDir(lookup, name); ok && e {
			add += "/"
		}
		return pathCompletion{add: add}
	}

	lcp := names[0]
	for _, nm := range names[1:] {
		for !strings.HasPrefix(nm, lcp) {
			lcp = lcp[:len(lcp)-1]
		}
	}
	if len(lcp) > len(prefix) {
		return pathCompletion{add: lcp[len(prefix):]}
	}
	return pathCompletion{list: names}
}

// entryIsDir reports whether lookup+name is a directory.
func entryIsDir(lookup, name string) (bool, bool) {
	info, err := os.Stat(lookup + name)
	if err != nil {
		return false, false
	}
	return info.IsDir(), true
}

// printMatches prints candidate names in columns.
// The terminal is in raw mode, so newlines are \r\n.
func printMatches(items []string) {
	const nameWidth = 26
	const perLine = 3
	for i, it := range items {
		display := it
		if len(display) > nameWidth {
			display = display[:nameWidth-1] + "~"
		}
		fmt.Printf("  %-*s", nameWidth, display)
		if (i+1)%perLine == 0 {
			fmt.Print("\r\n")
		}
	}
	if len(items)%perLine != 0 {
		fmt.Print("\r\n")
	}
}
