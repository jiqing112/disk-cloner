package cli

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
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

// ReadInputPath reads a file path from stdin in cooked terminal mode,
// preserving tab completion and OS line editing. Works on both platforms.
func ReadInputPath(prompt, def string) string {
	fmt.Printf("  %s: ", prompt)
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
		lower := strings.ToLower(input)
		if lower == "q" || lower == "quit" {
			return -2
		}
		if input == "" {
			fmt.Printf("  请输入 %d-%d，输入 q 返回上一步\n", minIdx, maxIdx)
			continue
		}
		idx, err := strconv.Atoi(input)
		if err != nil || idx < minIdx || idx > maxIdx {
			fmt.Printf("  请输入 %d-%d，输入 q 返回上一步\n", minIdx, maxIdx)
			continue
		}
		return idx
	}
}

func SelectOption(prompt string, minIdx, maxIdx int) int {
	for {
		input := ReadInput(prompt, "")
		lower := strings.ToLower(input)
		if lower == "q" || lower == "quit" {
			return -2
		}
		if input == "" {
			fmt.Printf("  请输入 %d-%d，输入 q 返回\n", minIdx, maxIdx)
			continue
		}
		idx, err := strconv.Atoi(input)
		if err != nil || idx < minIdx || idx > maxIdx {
			fmt.Printf("  请输入 %d-%d，输入 q 返回\n", minIdx, maxIdx)
			continue
		}
		return idx
	}
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
	input := ReadInput("  零填充 (回车=执行填充, 输入n=跳过填充) [Y/n]", "")
	lower := strings.ToLower(input)
	return lower != "n" && lower != "no"
}

// AskCompressionLevel prompts the user to choose gzip compression level.
func AskCompressionLevel() int {
	fmt.Println("  压缩级别:")
	fmt.Println("    0 = 不压缩 (局域网快速, 节省远程 CPU)")
	fmt.Println("    1 = 最快 (默认, 适合日常备份)")
	fmt.Println("    6 = 均衡 (中等压缩率)")
	fmt.Println("    9 = 最小 (最高压缩, 费 CPU)")
	fmt.Println("    回车使用默认值 1")
	input := ReadInput("  压缩级别", "1")
	val, err := strconv.Atoi(input)
	if err != nil || val < 0 || val > 9 {
		return 1
	}
	return val
}

// AskCompressionType prompts the user to choose compression algorithm.
// Returns 0 = gzip, 1 = pigz (multi-threaded).
func AskCompressionType() int {
	fmt.Println("  压缩方式:")
	fmt.Println("    1 = gzip (单核压缩, 兼容性最广)")
	fmt.Println("    2 = pigz (多核压缩, 速度更快, 需远程安装)")
	input := ReadInput("  压缩方式", "1")
	if input == "2" {
		return 1
	}
	return 0
}

// AskFixInitramfs asks whether to rebuild initramfs for cross-hardware boot.
func AskFixInitramfs() bool {
	fmt.Println("  重建 initramfs (兼容不同硬件启动)?")
	fmt.Println("    将远程系统的 initramfs 重建为包含所有硬件驱动")
	fmt.Println("    这样备份镜像恢复到其他机器(不同CPU/硬盘控制器)时也能正常启动")
	fmt.Println("    需要挂载远程分区并 chroot, 耗时约 1-2 分钟")
	input := ReadInput("  重建 initramfs [Y/n]", "y")
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
	transferred := formatSize(p.BytesWritten)
	elapsedStr := formatDuration(p.ElapsedSeconds)
	etaStr := "--"
	if p.EtaSeconds > 0 {
		etaStr = formatDuration(p.EtaSeconds)
	}

	if p.TotalBytes <= 0 {
		spinner := []string{"|", "/", "-", "\\"}
		sp := spinner[int(time.Now().UnixMilli()/200)%4]
		fmt.Printf("\r  [%s] %s  %6.1f MB/s  %s           ",
			sp, transferred, p.SpeedMBps, elapsedStr)
		flushConsole()
		return
	}

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

	total := formatSize(p.TotalBytes)

	fmt.Printf("\r  [%s] %5.1f%%  %6.1f MB/s  %s/%s  %s  ETA: %s          ",
		string(bar), p.Percent, p.SpeedMBps, transferred, total, elapsedStr, etaStr)
	flushConsole()
}

func PrintProgressComplete(p CloneProgress) {
	fmt.Print("\r                                                                                                    \r")
	duration := formatDuration(p.ElapsedSeconds)
	fmt.Printf("  完成!  %s 已传输  平均速度: %.1f MB/s  用时: %s\n",
		formatSize(p.BytesWritten), p.SpeedMBps, duration)
	fmt.Println()
}

// flushConsole flushes stdout to show progress in real-time.
func flushConsole() {
	// Use a single reusable channel to limit goroutines
	// On Windows, Sync() can block when window is minimized.
	// We run it in a goroutine so the main loop isn't blocked,
	// but cap it to avoid goroutine accumulation.
	select {
	case flushCh <- struct{}{}:
	default:
		// Previous flush still running, skip this one
	}
}

var flushCh = make(chan struct{}, 1)

func init() {
	go func() {
		for range flushCh {
			os.Stdout.Sync()
		}
	}()
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
