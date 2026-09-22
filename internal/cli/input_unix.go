//go:build !windows

package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// pending holds stdin bytes read ahead of the current keystroke. A single
// Read in raw mode can return a whole pasted chunk (multi-line text, escape
// sequences, multi-byte UTF-8); bytes are queued here and handed to the key
// handler one at a time so embedded \r/\t/\x7f are processed as keys instead
// of becoming literals inside the input (and pasted newlines stop merging
// lines together).
var pending []byte

// readKey returns the next keystroke byte, refilling the pending queue from
// stdin when it runs dry.
func readKey(buf []byte) (byte, error) {
	if len(pending) == 0 {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return 0, err
		}
		pending = append(pending, buf[:n]...)
	}
	b := pending[0]
	pending = pending[1:]
	return b, nil
}

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
		b, rerr := readKey(oneByte)
		if rerr != nil {
			if len(buf) == 0 {
				stdinEOF = true
			}
			break
		}

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
			// UTF-8 sequences arrive byte-by-byte via readKey and are
			// appended/echoed per byte — the terminal renders the
			// multi-byte rune correctly as the bytes stream in.
			buf = append(buf, b)
			fmt.Print(string(b))

		case b == 27:
			readEscapeFollowup()

		default:
		}
	}

	input := strings.TrimSpace(string(buf))
	if input == "" {
		return def
	}
	return input
}

// pipedBuf holds stdin data read ahead of the current line when input is a
// pipe/redirect. A single Read can return several lines at once; without
// this buffer everything after the first newline was silently dropped, so
// `printf '1\n2\n' | tool` never saw the second line.
var pipedBuf []byte

// readLineSimple is a fallback for non-terminal stdin (pipes, redirects).
// Data that arrives without a trailing newline (EOF) is still returned.
func readLineSimple(def string) string {
	for {
		for i := 0; i < len(pipedBuf); i++ {
			if pipedBuf[i] == '\n' || pipedBuf[i] == '\r' {
				input := strings.TrimSpace(string(pipedBuf[:i]))
				pipedBuf = pipedBuf[i+1:]
				if input == "" {
					return def
				}
				return input
			}
		}
		var chunk [4096]byte
		n, err := os.Stdin.Read(chunk[:])
		pipedBuf = append(pipedBuf, chunk[:n]...)
		if err != nil {
			if n == 0 && len(pipedBuf) == 0 {
				stdinEOF = true
				return def
			}
			// EOF (or error) before a newline: use what we got.
			input := strings.TrimSpace(string(pipedBuf))
			pipedBuf = nil
			if input == "" {
				return def
			}
			return input
		}
	}
}

// ReadPassword reads a password without echoing to the terminal.
func ReadPassword(prompt string) string {
	fmt.Printf("  %s: ", prompt)
	pass, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return ""
	}
	// Only the line terminator is stripped: leading/trailing spaces are
	// legal password characters, and silently trimming them produces auth
	// failures that are nearly impossible to diagnose.
	return strings.TrimRight(string(pass), "\r\n")
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
		b, rerr := readKey(oneByte)
		if rerr != nil {
			if len(buf) == 0 {
				stdinEOF = true
			}
			break
		}

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
			buf = append(buf, b)
			fmt.Print(string(b))

		case b == 27:
			readEscapeFollowup()

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
		if n > 1 {
			pending = append(pending, oneByte[1:n]...) // keep pasted extras
		}
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

// readEscapeFollowup consumes up to 2 bytes of a terminal escape sequence
// after ESC, preferring bytes that already sit in the pending queue. It
// only reads more from the terminal when bytes are already available
// (50ms window), so pressing Esc alone no longer blocks the prompt waiting
// for input that never comes.
func readEscapeFollowup() int {
	consumed := 0
	for consumed < 2 && len(pending) > 0 {
		pending = pending[1:]
		consumed++
	}
	if consumed >= 2 {
		return consumed
	}
	fds := []unix.PollFd{{Fd: int32(os.Stdin.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 50)
	if err != nil || n == 0 {
		return consumed
	}
	// Read at most what's missing from the sequence; anything past it
	// stays in the kernel buffer for the next readKey round.
	var tmp [2]byte
	n2, _ := os.Stdin.Read(tmp[:2-consumed])
	return consumed + n2
}

// printMatches prints candidate names in columns.
// The terminal is in raw mode, so newlines are \r\n.
func printMatches(items []string) {
	const nameWidth = 26
	const perLine = 3
	for i, it := range items {
		display := it
		// Truncate by rune, never mid-sequence (CJK filenames).
		if runes := []rune(display); len(runes) > nameWidth-1 {
			display = string(runes[:nameWidth-1]) + "~"
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
