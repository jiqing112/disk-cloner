package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

type DiskItem struct {
	Path      string
	SizeHuman string
	SizeBytes int64
	Model     string
	Name      string // disk name without /dev/ prefix, e.g. "sda"
}

func PrintHeader() {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║       Disk Cloner - 磁盘克隆工具 v3               ║")
	fmt.Println("║       远程 → 本地 dd 克隆 (Alpine Linux)          ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()
}

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
		return readLine(def)
	}

	// Switch to raw mode for proper backspace handling
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		// Fallback to simple read
		return readLine(def)
	}
	defer term.Restore(fd, oldState)

	var buf []byte
	oneByte := make([]byte, 4) // buffer for multi-byte chars

	for {
		n, err := os.Stdin.Read(oneByte)
		if err != nil || n == 0 {
			break
		}

		b := oneByte[0]

		switch {
		case b == '\r' || b == '\n':
			// Enter — done
			fmt.Print("\r\n")
			input := strings.TrimSpace(string(buf))
			if input == "" {
				return def
			}
			return input

		case b == 3:
			// Ctrl+C
			fmt.Print("\r\n")
			term.Restore(fd, oldState)
			os.Exit(0)

		case b == 127 || b == 8:
			// Backspace (127) or Ctrl+H (8)
			if len(buf) > 0 {
				// Remove last rune (may be multi-byte)
				_, size := utf8.DecodeLastRune(buf)
				buf = buf[:len(buf)-size]
				// Erase character on screen: move back, space, move back
				fmt.Print("\b \b")
			}

		case b == 21:
			// Ctrl+U — clear entire line
			for len(buf) > 0 {
				_, size := utf8.DecodeLastRune(buf)
				buf = buf[:len(buf)-size]
				fmt.Print("\b \b")
			}

		case b == 23:
			// Ctrl+W — delete last word
			for len(buf) > 0 {
				_, size := utf8.DecodeLastRune(buf)
				ch, _ := utf8.DecodeLastRune(buf)
				buf = buf[:len(buf)-size]
				fmt.Print("\b \b")
				if ch == ' ' {
					break
				}
			}

		case b >= 32:
			// Printable character (or start of multi-byte UTF-8)
			if n == 1 && b < 128 {
				buf = append(buf, b)
				fmt.Print(string(b))
			} else {
				// Multi-byte UTF-8: first byte already read, read continuation bytes
				char := oneByte[:n]
				buf = append(buf, char...)
				fmt.Print(string(char))
			}

		case b == 27:
			// Escape sequence (arrow keys etc.) — consume and ignore
			os.Stdin.Read(oneByte[:2])

		default:
			// Ignore other control characters
		}
	}

	input := strings.TrimSpace(string(buf))
	if input == "" {
		return def
	}
	return input
}

// readLine is a simple fallback for non-terminal stdin
func readLine(def string) string {
	var buf [4096]byte
	total := 0
	for {
		n, err := os.Stdin.Read(buf[total:])
		total += n
		if err != nil || total == 0 {
			return def
		}
		// Check for newline
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

func ReadPassword(prompt string) string {
	fmt.Printf("  %s: ", prompt)
	pass, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(pass))
}

func ReadInt(prompt string, def int) int {
	str := ReadInput(prompt, strconv.Itoa(def))
	val, err := strconv.Atoi(str)
	if err != nil || val <= 0 {
		return def
	}
	return val
}

func PrintSection(title string) {
	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════")
	fmt.Printf("  %s\n", title)
	fmt.Println("═══════════════════════════════════════════════════")
}

func PrintDiskList(disks []DiskItem, location string) {
	for i, d := range disks {
		idx := i + 1
		model := ""
		if d.Model != "" {
			model = "  " + d.Model
		}
		fmt.Printf("  [%d] %-16s %8s%s\n", idx, d.Path, d.SizeHuman, model)
	}
}

func SelectDisk(prompt string, minIdx, maxIdx int) int {
	for {
		input := ReadInput(prompt, "")
		if input == "" {
			fmt.Printf("  请输入 %d-%d 之间的数字\n", minIdx, maxIdx)
			continue
		}
		idx, err := strconv.Atoi(input)
		if err != nil || idx < minIdx || idx > maxIdx {
			fmt.Printf("  请输入 %d-%d 之间的数字\n", minIdx, maxIdx)
			continue
		}
		return idx
	}
}

func SelectOption(prompt string, minIdx, maxIdx int) int {
	return SelectDisk(prompt, minIdx, maxIdx)
}

func Confirm(prompt string) bool {
	input := ReadInput(prompt, "")
	lower := strings.ToLower(input)
	return lower == "yes" || lower == "y"
}

type CloneProgress struct {
	BytesWritten   int64
	TotalBytes     int64
	Percent        float64
	SpeedMBps      float64
	ElapsedSeconds int64
	EtaSeconds     int64
}

func PrintProgress(p CloneProgress) {
	barWidth := 40
	filled := int(p.Percent / 100 * float64(barWidth))
	if filled < 0 {
		filled = 0
	}
	if filled > barWidth {
		filled = barWidth
	}

	bar := make([]byte, barWidth)
	for i := range bar {
		if i < filled {
			bar[i] = '='
		} else if i == filled && filled < barWidth {
			bar[i] = '>'
		} else {
			bar[i] = '-'
		}
	}

	etaStr := "--"
	if p.EtaSeconds > 0 {
		etaStr = formatDuration(p.EtaSeconds)
	}

	transferred := formatSize(p.BytesWritten)
	total := formatSize(p.TotalBytes)

	fmt.Printf("\r\033[K  [%s] %5.1f%%  %6.1f MB/s  %s/%s  ETA: %s",
		string(bar), p.Percent, p.SpeedMBps, transferred, total, etaStr)
}

func PrintProgressComplete(p CloneProgress) {
	fmt.Printf("\r\033[K")
	duration := formatDuration(p.ElapsedSeconds)
	fmt.Printf("  ✓ 完成!  %s 已传输  平均速度: %.1f MB/s  用时: %s\n",
		formatSize(p.BytesWritten), p.SpeedMBps, duration)
	fmt.Println()
}

func formatDuration(seconds int64) string {
	if seconds <= 0 {
		return "0s"
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm%ds", seconds/60, seconds%60)
	}
	h := seconds / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	return fmt.Sprintf("%dh%dm%ds", h, m, s)
}

func formatSize(bytes int64) string {
	if bytes <= 0 {
		return "0 B"
	}
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
