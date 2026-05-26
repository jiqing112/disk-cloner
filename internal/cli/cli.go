package cli

import (
	"fmt"
	"strconv"
	"strings"
)

type DiskItem struct {
	Path      string
	SizeHuman string
	SizeBytes int64
	Model     string
	Name      string
}

func PrintHeader() {
	fmt.Println()
	fmt.Println("+==============================================+")
	fmt.Println("|       Disk Cloner - 磁盘克隆工具 v3           |")
	fmt.Println("|    远程 -> 本地 dd 克隆 (Alpine Linux)        |")
	fmt.Println("+==============================================+")
	fmt.Println()
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
	fmt.Println("===============================================")
	fmt.Printf("  %s\n", title)
	fmt.Println("===============================================")
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

// ConfirmZero asks whether to zero-fill free space before clone.
// Default is Yes (just press Enter). Returns false if user types "n" or "no".
func ConfirmZero() bool {
	fmt.Println("  传输前先零填充空闲空间?")
	fmt.Println("    将空闲空间写零可大幅提高压缩率，减少网络传输量。")
	fmt.Println("    可能需要较长时间，但能显著减少网络流量。")
	input := ReadInput("  零填充 [Y/n]", "y")
	lower := strings.ToLower(input)
	return lower != "n" && lower != "no"
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

	fmt.Printf("\r  [%s] %5.1f%%  %6.1f MB/s  %s/%s  ETA: %s          ",
		string(bar), p.Percent, p.SpeedMBps, transferred, total, etaStr)
}

func PrintProgressComplete(p CloneProgress) {
	fmt.Print("\r                                                                                                    \r")
	duration := formatDuration(p.ElapsedSeconds)
	fmt.Printf("  完成!  %s 已传输  平均速度: %.1f MB/s  用时: %s\n",
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
		return fmt.Sprintf("%d分%d秒", seconds/60, seconds%60)
	}
	h := seconds / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	return fmt.Sprintf("%d时%d分%d秒", h, m, s)
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
