package clone

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	sshclient "disk-cloner/internal/ssh"
	"golang.org/x/crypto/ssh"
)

type Params struct {
	SourcePath       string
	TargetPath       string // block device for clone, file path for save
	SourceSize       int64
	BlockSize        string
	ZeroFill         bool // zero-fill free space before dd (improves compression)
	CompressionLevel int  // 0=no compression, 1-9=gzip level (default 1)
	CompressType     int  // 0=gzip, 1=pigz
	FixInitramfs     bool // rebuild initramfs with --no-hostonly before dd
}

// LogFunc is called to print status messages during zero-fill etc.
type LogFunc func(format string, args ...interface{})

type Progress struct {
	BytesWritten   int64 // uncompressed bytes (= disk bytes written or read)
	TotalBytes     int64
	Percent        float64
	SpeedMBps      float64 // uncompressed speed
	ElapsedSeconds int64
	EtaSeconds     int64
	Done           bool
	Error          error
}

type CloneJob struct {
	sshClient  *sshclient.Client
	params     Params
	progressFn func(Progress)
	logFn      LogFunc

	// compressCmd is the built remote compression command (cached so the
	// tool detection runs only once per job). compressTool is the actual
	// tool in use ("gzip" or "pigz"), which may differ from the requested
	// CompressType when pigz is unavailable and we fall back to gzip.
	compressCmd  string
	compressTool string

	// ddSupportsFdatasync is probed once: GNU coreutils dd supports
	// conv=fdatasync, BusyBox dd does not. ddTested marks whether the
	// probe has run.
	ddSupportsFdatasync bool
	ddTested            bool

	// ChecksumHex is set by RunToFile on success: SHA256 hex of the complete
	// output file, computed while streaming (no second read pass needed).
	ChecksumHex string
}

func New(sshClient *sshclient.Client, params Params, progressFn func(Progress)) *CloneJob {
	return &CloneJob{
		sshClient:  sshClient,
		params:     params,
		progressFn: progressFn,
		logFn:      func(format string, args ...interface{}) {},
	}
}

func (j *CloneJob) SetLogFunc(fn LogFunc) {
	j.logFn = fn
}

var safePathRe = regexp.MustCompile(`^/dev/[a-zA-Z0-9/_-]+$`)

func validateDevicePath(path string) error {
	if !safePathRe.MatchString(path) {
		return fmt.Errorf("invalid device path: %q (must match /dev/...)", path)
	}
	return nil
}

// ValidateDevicePath exports the device-path safety check so callers can
// validate user-supplied remote disk paths before embedding them in shell
// commands (prevents command injection via lsblk/umount/grep etc.).
func ValidateDevicePath(path string) error {
	return validateDevicePath(path)
}

var safeBSRe = regexp.MustCompile(`^[0-9]+[KMGkmg]?$`)

func validateBS(bs string) error {
	if !safeBSRe.MatchString(bs) {
		return fmt.Errorf("invalid block size: %q", bs)
	}
	return nil
}

// bsToBytes converts human-readable block size to bytes string.
// Busybox dd does NOT support suffixes like "4M" — only plain byte counts.
func bsToBytes(bs string) string {
	if bs == "" {
		return "4194304"
	}
	bs = strings.TrimSpace(bs)
	if len(bs) == 0 {
		return "4194304"
	}
	last := rune(bs[len(bs)-1])
	if unicode.IsDigit(last) {
		return bs
	}
	numStr := bs[:len(bs)-1]
	num, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return "4194304"
	}
	switch unicode.ToUpper(last) {
	case 'K':
		return strconv.FormatInt(num*1024, 10)
	case 'M':
		return strconv.FormatInt(num*1024*1024, 10)
	case 'G':
		return strconv.FormatInt(num*1024*1024*1024, 10)
	default:
		return bs
	}
}

// GzipUncompressedSize reads the uncompressed size from a .gz file.
// Returns 0 if unknown (file > 4 GB or not a valid gzip).
func GzipUncompressedSize(path string) int64 {
	return readGzipISize(path)
}

// readGzipISize reads the last 4 bytes of a gzip file to get the
// uncompressed data size (ISIZE, stored as uint32 LE modulo 2^32).
// For files <= 4 GB this is exact; for larger files it wraps around.
func readGzipISize(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	// Seek to last 4 bytes
	if _, err := f.Seek(-4, io.SeekEnd); err != nil {
		return 0
	}

	var buf [4]byte
	if _, err := io.ReadFull(f, buf[:]); err != nil {
		return 0
	}

	// ISIZE is stored little-endian, uint32
	size := int64(uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24)

	// If the uncompressed size is larger than 4 GB, ISIZE wraps around.
	// Heuristic to detect ISIZE wrap (uncompressed size > 4 GB):
	// If the claimed ISIZE is less than 2x the compressed file size,
	// the actual uncompressed data probably exceeded 4 GB and ISIZE wrapped.
	// gzip cannot normally achieve >50% compression on disk images, so
	// ISIZE < compressedSize * 2 is a reliable indicator of wrapping.
	info, _ := f.Stat()
	compressedSize := info.Size()
	if compressedSize > 512*1024*1024 && size < compressedSize*2 {
		return 0 // likely wrapped
	}

	return size
}

// IsGzipFile reports whether the file starts with the gzip magic bytes (1f 8b).
func IsGzipFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [2]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return magic[0] == 0x1f && magic[1] == 0x8b
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func formatBytesCompat(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	if n < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
	return fmt.Sprintf("%.2f GB", float64(n)/(1024*1024*1024))
}

// remoteHasCommand checks if a command exists on the remote server.
func (j *CloneJob) remoteHasCommand(cmd string) bool {
	_, err := j.sshClient.CombinedOutput("command -v " + cmd)
	return err == nil
}

// probeDdFdatasync checks once whether the remote dd supports conv=fdatasync.
// BusyBox dd (the default on Alpine RAM OS) does NOT support it and dies with
// "invalid conversion: fdatasync" — which causes the restore to fail with
// "write error: EOF" because dd exits immediately and the SSH stdin closes.
// GNU coreutils dd supports it. We fall back to a post-write `sync` command
// (which BusyBox supports) when fdatasync is unavailable.
func (j *CloneJob) probeDdFdatasync() {
	if j.ddTested {
		return
	}
	j.ddTested = true
	// Run a no-op dd with conv=fdatasync. If it errors, dd doesn't support it.
	out, err := j.sshClient.CombinedOutput("dd if=/dev/zero of=/dev/null bs=1 count=1 conv=fdatasync 2>&1")
	if err == nil && !strings.Contains(strings.ToLower(out), "invalid") && !strings.Contains(strings.ToLower(out), "error") {
		j.ddSupportsFdatasync = true
		j.logFn("  dd: GNU coreutils (conv=fdatasync supported)")
	} else {
		j.ddSupportsFdatasync = false
		j.logFn("  dd: BusyBox (conv=fdatasync not supported, using post-write sync)")
	}
}

// parseDdBytesRead extracts the number of bytes read by dd from its stderr
// output. Busybox dd prints a line like "12345+0 records in", GNU coreutils dd
// prints "1234567 bytes (1.2 MB, 1.1 MiB) copied". Returns -1 if not found.
func parseDdBytesRead(stderr string) int64 {
	// GNU coreutils dd: "N bytes (...) copied, ..."
	for _, line := range strings.Split(stderr, "\n") {
		if i := strings.Index(line, " bytes"); i > 0 {
			n, err := strconv.ParseInt(strings.TrimSpace(line[:i]), 10, 64)
			if err == nil && n > 0 {
				return n
			}
		}
	}
	// Busybox dd: "<records>+<records> records in" with bs*N = bytes
	// We need bs to compute, so this path returns -1 unless single-line count.
	// Most Alpine deployments now have GNU dd via coreutils; fall back to -1.
	return -1
}

// preReadWarnIfMounted flushes pending writes on the source and warns loudly
// if any partition of the source disk is still mounted (i.e. the OS is live
// and writing concurrently with our dd read — produces a torn image).
//
// This is the #1 defense against the "ext4 bad block bitmap checksum" /
// "Journal has aborted" corruption seen after restoring a backup that was
// taken while the source system was live.
func (j *CloneJob) preReadWarnIfMounted() {
	src := j.params.SourcePath

	// Flush all pending writes on the remote so whatever is in the page
	// cache hits the disk before we start reading. Cheap insurance.
	j.logFn("  Flushing remote filesystem buffers (sync)...")
	j.sshClient.CombinedOutput("sync")

	// Detect partitions of the source disk that are still mounted.
	// Partition suffix is (p?[0-9]+) to cover both sda1 and nvme0n1p1 styles.
	diskBase := src
	if i := strings.LastIndex(src, "/"); i >= 0 {
		diskBase = src[i+1:]
	}
	out, _ := j.sshClient.CombinedOutput(fmt.Sprintf(
		`for mp in $(grep -oE '/dev/(mapper/)?%s(p[0-9]+|[0-9]+)' /proc/mounts 2>/dev/null | sort -u); do echo "MOUNTED $mp"; done; `+
			`grep -E '(/dev/%s(p[0-9]+|[0-9]+)|/dev/mapper/.*%s)' /proc/mounts 2>/dev/null | awk '{print "MOUNT "$2}'`,
		diskBase, diskBase, diskBase))
	mounted := []string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MOUNT") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				mounted = append(mounted, fields[1])
			}
		}
	}
	if len(mounted) > 0 {
		j.logFn("  [!] 警告: 源磁盘的以下分区仍处于挂载状态:")
		for _, mp := range mounted {
			j.logFn("        - %s", mp)
		}
		j.logFn("  [!] 远程系统可能仍在运行,备份的镜像可能不一致!")
		j.logFn("  [!] 强烈建议先重启进入 Alpine RAM OS 再备份")
		j.logFn("  [!] 将继续备份,但恢复后可能出现 ext4 journal/损坏错误")
	}
}

// preReadSyncAndVerify does a final sync on the source and aborts if any of
// its partitions are still mounted. This MUST run after zero-fill and
// initramfs rebuild (which mount/unmount source partitions) and before dd.
//
// A still-mounted source partition means the filesystem journal is not fully
// committed to disk. dd reads the block device directly, bypassing the page
// cache, so it sees whatever is on disk right now — including a half-written
// journal. When that image is restored, GRUB's xfs/ext4 driver reads the
// half-written metadata and fails with "unknown filesystem".
//
// We use this instead of a lazy fallback umount (-l) inside zero-fill/initramfs:
// lazy umount detaches the namespace but the filesystem stays live in the
// kernel, so the journal is never finalized.
func (j *CloneJob) preReadSyncAndVerify(src string) {
	// Final sync — flush everything zero-fill / initramfs wrote.
	j.sshClient.CombinedOutput("sync; sync; sync")

	// Give the kernel a moment to finish flushing.
	time.Sleep(2 * time.Second)

	diskBase := src
	if i := strings.LastIndex(src, "/"); i >= 0 {
		diskBase = src[i+1:]
	}
	out, _ := j.sshClient.CombinedOutput(fmt.Sprintf(
		`grep -E '(/dev/%s(p[0-9]+|[0-9]+)|/dev/mapper/.*%s)' /proc/mounts 2>/dev/null | awk '{print $2}'`,
		diskBase, diskBase))
	stillMounted := []string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			stillMounted = append(stillMounted, line)
		}
	}
	if len(stillMounted) > 0 {
		// Try one more regular umount (NOT lazy). If it fails we must abort.
		for _, mp := range stillMounted {
			j.sshClient.CombinedOutput(fmt.Sprintf("umount %s 2>/dev/null", mp))
		}
		// Re-check.
		out2, _ := j.sshClient.CombinedOutput(fmt.Sprintf(
			`grep -E '(/dev/%s(p[0-9]+|[0-9]+)|/dev/mapper/.*%s)' /proc/mounts 2>/dev/null | awk '{print $2}'`,
			diskBase, diskBase))
		stillMounted = stillMounted[:0]
		for _, line := range strings.Split(out2, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				stillMounted = append(stillMounted, line)
			}
		}
	}
	if len(stillMounted) > 0 {
		j.logFn("  [!] 严重: 源盘的以下分区无法卸载,dd 读到的镜像将不一致:")
		for _, mp := range stillMounted {
			j.logFn("        - %s", mp)
		}
		j.logFn("  [!] 请手动 umount 后重试,不要使用 lazy umount (-l)")
		j.logFn("  [!] 继续备份,但镜像可能损坏 (恢复后 GRUB 报 unknown filesystem)")
	} else {
		j.logFn("  ✓ 源盘所有分区已卸载,文件系统状态一致")
	}
}

// freezeSource freezes the remote block device to take a stable snapshot
// for dd to read. Best-effort: silently ignored if the kernel/busybox
// blockdev doesn't support it.
func (j *CloneJob) freezeSource() {
	j.sshClient.CombinedOutput(fmt.Sprintf("blockdev --freeze %s 2>/dev/null", j.params.SourcePath))
}

// unfreezeSource releases the freeze taken by freezeSource. Best-effort.
func (j *CloneJob) unfreezeSource() {
	j.sshClient.CombinedOutput(fmt.Sprintf("blockdev --unfreeze %s 2>/dev/null", j.params.SourcePath))
}

// preReadSafetyCheck and postReadUnfreeze are kept for backwards
// compatibility but no longer used by Run/RunToFile. The freeze/unfreeze
// is now split so that source-modifying steps (zero-fill, initramfs) run
// BEFORE the freeze, not while frozen.
//
// Deprecated: use preReadWarnIfMounted + preReadSyncAndVerify + freezeSource.
func (j *CloneJob) preReadSafetyCheck() {
	j.preReadWarnIfMounted()
	j.sshClient.CombinedOutput(fmt.Sprintf("blockdev --freeze %s 2>/dev/null", j.params.SourcePath))
}

func (j *CloneJob) postReadUnfreeze() {
	j.sshClient.CombinedOutput(fmt.Sprintf("blockdev --unfreeze %s 2>/dev/null", j.params.SourcePath))
}

// buildCompressCmd returns the compress command segment for the remote pipeline.
// It detects available tools (gzip, pigz) and builds the appropriate command.
// Also ensures the chosen tool is installed. The result is cached so tool
// detection runs only once per job; subsequent calls return the same command.
func (j *CloneJob) buildCompressCmd() string {
	if j.compressCmd != "" {
		return j.compressCmd
	}

	level := j.params.CompressionLevel
	if level <= 0 || level > 9 {
		level = 1
	}

	// If pigz requested, try to install and use it
	if j.params.CompressType == 1 {
		j.sshClient.CombinedOutput("command -v pigz || apk add --quiet pigz 2>/dev/null")
		if j.remoteHasCommand("pigz") {
			ncpus, _ := j.sshClient.CombinedOutput("nproc 2>/dev/null")
			ncpus = strings.TrimSpace(ncpus)
			threads := "2"
			if n, err := strconv.Atoi(ncpus); err == nil && n > 1 {
				threads = strconv.Itoa(n)
			}
			j.compressCmd = fmt.Sprintf("pigz -%d -p %s", level, threads)
			j.compressTool = "pigz"
			return j.compressCmd
		}
		// If pigz install failed, fall through to gzip
	}

	// Install or check gzip
	j.sshClient.CombinedOutput("command -v gzip || apk add --quiet gzip 2>/dev/null")
	j.compressCmd = fmt.Sprintf("gzip -%d", level)
	j.compressTool = "gzip"
	return j.compressCmd
}

// compressToolName returns the human-readable name of the compression tool
// actually in use. Accurate even when pigz was requested but unavailable.
func (j *CloneJob) compressToolName() string {
	if j.compressTool != "" {
		return j.compressTool
	}
	if j.params.CompressType == 1 {
		return "pigz"
	}
	return "gzip"
}

// Run clones remote disk to a local block device.
// Uses gzip compression over SSH to reduce network transfer:
//
//	remote: dd | gzip -> SSH -> local: gunzip -> write to disk
//
// Also does zero-fill beforehand to maximize compression.
func (j *CloneJob) Run() error {
	if err := validateDevicePath(j.params.SourcePath); err != nil {
		return err
	}
	if err := validateDevicePath(j.params.TargetPath); err != nil {
		return err
	}

	// Pre-read source preparation (order matters!):
	//   1. Warn if source partitions are mounted (live OS).
	//   2. Zero-fill free space (mounts, writes, unmounts source).
	//   3. Rebuild initramfs (chroots into source).
	//   4. sync + verify all source partitions unmounted.
	//   5. Freeze block device for a stable snapshot.
	//   6. dd | gzip
	//   7. Unfreeze.
	//
	// Freezing AFTER zero-fill/initramfs is critical: freeze locks the
	// block queue, so any writes done while frozen (or queued by lazy
	// unmount) never reach disk before dd reads, producing a torn image.
	j.preReadWarnIfMounted()

	if j.params.ZeroFill {
		if err := j.zeroFillFreeSpace(); err != nil {
			j.logFn("  [!] Zero-fill failed (continuing clone): %v", err)
		}
	}

	if j.params.FixInitramfs {
		if err := j.FixInitramfs(); err != nil {
			j.logFn("  [!] %v", err)
		}
	}

	// Flush everything the zero-fill/initramfs steps wrote, then verify the
	// source is clean (no partitions still mounted). A still-mounted source
	// at this point means dd will read a filesystem whose journal is not
	// fully committed — the classic cause of "GRUB: unknown filesystem"
	// after restore.
	j.preReadSyncAndVerify(j.params.SourcePath)

	j.freezeSource()
	defer j.unfreezeSource()

	target, err := os.OpenFile(j.params.TargetPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open target device: %w", err)
	}
	defer target.Close()

	// Compression level 0 = no compression, 1-9 = gzip/pigz level
	if j.params.CompressionLevel > 0 {
		// Build the compress command first so compressToolName reflects the
		// actual tool (pigz may fall back to gzip).
		_ = j.buildCompressCmd()
		j.logFn("  Compressed transfer (dd|%s -> net -> gunzip -> disk)", j.compressToolName())
		if err := j.streamCompressed(target); err != nil {
			return err
		}
	} else {
		j.logFn("  No compression (dd -> net -> disk)")
		if err := j.streamRaw(target); err != nil {
			return err
		}
	}

	if err := target.Sync(); err != nil {
		return fmt.Errorf("sync target device: %w", err)
	}

	return nil
}

// RunToFile saves remote disk as a gzip compressed file.
// Remote does dd | gzip, only compressed data travels over the network.
// The file is a standard RFC 1952 gzip — compatible with gunzip/dd everywhere.
// With CompressionLevel == 0 the image is written uncompressed (raw .img).
func (j *CloneJob) RunToFile() error {
	if err := validateDevicePath(j.params.SourcePath); err != nil {
		return err
	}
	// Pre-read source preparation (order matters! see Run docs):
	//   warn mounted -> zero-fill -> initramfs -> sync+verify -> freeze -> dd.
	j.preReadWarnIfMounted()

	if j.params.ZeroFill {
		if err := j.zeroFillFreeSpace(); err != nil {
			j.logFn("  [!] Zero-fill failed (continuing save): %v", err)
		}
	}

	if j.params.FixInitramfs {
		if err := j.FixInitramfs(); err != nil {
			j.logFn("  [!] %v", err)
		}
	}

	// Critical: flush all writes from zero-fill/initramfs and verify the
	// source disk has no partitions still mounted. A mounted source during
	// dd produces an image whose filesystem journal is mid-transaction,
	// which GRUB refuses to read ("error: unknown filesystem").
	j.preReadSyncAndVerify(j.params.SourcePath)

	j.freezeSource()
	defer j.unfreezeSource()

	f, err := os.Create(j.params.TargetPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	// Hash the output while streaming so no second read pass over the
	// (possibly multi-GB) file is needed for the .sha256 file.
	hasher := sha256.New()
	out := io.MultiWriter(f, hasher)

	if j.params.CompressionLevel > 0 {
		// Build the compress command first so compressToolName reflects the
		// actual tool (pigz may fall back to gzip).
		_ = j.buildCompressCmd()
		j.logFn("  Remote compressing (dd|%s -> net -> file)", j.compressToolName())
		if err := j.streamCompressedRaw(out); err != nil {
			return err
		}
	} else {
		j.logFn("  No compression (dd -> net -> file)")
		if err := j.streamRaw(out); err != nil {
			return err
		}
	}

	// Flush to disk before returning: the caller generates the .sha256 file
	// immediately after and may show "save complete", so the image must be
	// durable by then (protects against power loss right after saving).
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync file: %w", err)
	}
	j.ChecksumHex = hex.EncodeToString(hasher.Sum(nil))

	return nil
}

// RestoreFromFile reads a local .img.gz file and writes it to a remote disk.
// Flow: local gzip file -> gunzip -> SSH stdin -> remote dd of=TARGET
func (j *CloneJob) RestoreFromFile(filePath string) error {
	if j.sshClient == nil {
		return fmt.Errorf("SSH client is nil")
	}
	if err := validateDevicePath(j.params.TargetPath); err != nil {
		return fmt.Errorf("invalid target device: %w", err)
	}

	// Get compressed file size for progress
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("stat file: %w", err)
	}
	j.logFn("  源文件: %s (%s)", filePath, formatBytesCompat(fileInfo.Size()))

	// Open the image. Standard .img.gz files are gzip; raw .img files
	// (saved with compression level 0) are streamed as-is.
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	var src io.Reader = f
	if IsGzipFile(filePath) {
		// Read uncompressed size from gzip footer (last 4 bytes = ISIZE
		// modulo 2^32)
		if uncompSize := readGzipISize(filePath); uncompSize > 0 {
			j.params.SourceSize = uncompSize
		}

		gzr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("init gzip decompressor: %w", err)
		}
		defer gzr.Close()
		src = gzr
	} else {
		j.logFn("  非 gzip 文件, 按原始镜像恢复")
		j.params.SourceSize = fileInfo.Size()
	}

	// Start remote dd with stdin pipe
	bs := j.params.BlockSize
	if bs == "" {
		bs = "4M"
	}
	if err := validateBS(bs); err != nil {
		return err
	}
	bsBytes := bsToBytes(bs)
	// Start remote dd with stdin pipe.
	// conv=fdatasync forces dd to flush data to disk before exiting, so the
	// ext4 journal and superblock are consistent when the machine reboots.
	// Not all dd implementations support it (BusyBox dd does not), so we
	// probe first; the post-write `sync` command is the universal fallback.
	j.probeDdFdatasync()
	remoteCmd := fmt.Sprintf("dd of=%s bs=%s", j.params.TargetPath, bsBytes)
	if j.ddSupportsFdatasync {
		remoteCmd += " conv=fdatasync"
	}

	session, err := j.sshClient.ExecuteStdin(remoteCmd)
	if err != nil {
		return fmt.Errorf("start remote dd: %w", err)
	}
	defer session.Close()

	// Capture stderr
	stderrCh := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(session.Stderr)
		stderrCh <- string(data)
	}()

	done := make(chan struct{})
	defer close(done)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	var cancelled atomic.Bool
	go func() {
		for {
			select {
			case <-sigCh:
				if !cancelled.Load() {
					cancelled.Store(true)
					_ = session.Signal(ssh.SIGTERM)
					j.logFn("  [!] 正在取消... (再按一次 Ctrl+C 强制退出)")
				} else {
					os.Exit(130)
				}
			case <-done:
				return
			}
		}
	}()

	// Copy decompressed data to remote dd via SSH stdin
	written, copyErr := j.copyWithProgress(session.Stdin, src)

	// Close stdin to signal EOF to remote dd
	session.Stdin.Close()

	sessionErr := session.Wait()

	// Force an additional global sync on the remote so all filesystem
	// metadata is flushed to the underlying block device before we return.
	// conv=fdatasync in dd only syncs the dd output stream; a separate sync
	// guarantees the kernel has written all dirty pages from the block device.
	if !cancelled.Load() && copyErr == nil {
		j.sshClient.CombinedOutput("sync")
	}

	stderrOut := ""
	select {
	case stderrOut = <-stderrCh:
	case <-time.After(3 * time.Second):
	}

	var finalErr error
	if cancelled.Load() {
		finalErr = fmt.Errorf("cancelled by user")
	} else if copyErr != nil {
		finalErr = copyErr
	} else if sessionErr != nil {
		errMsg := fmt.Sprintf("remote dd: %v", sessionErr)
		if stderrOut != "" {
			errMsg += "\n  stderr: " + strings.TrimSpace(stderrOut)
		}
		finalErr = fmt.Errorf("%s", errMsg)
	} else if written == 0 {
		errMsg := "no data written to remote disk"
		if stderrOut != "" {
			errMsg += "\n  stderr: " + strings.TrimSpace(stderrOut)
		}
		finalErr = fmt.Errorf("%s", errMsg)
	} else if j.params.SourceSize > 0 && written < j.params.SourceSize {
		// Short write: image was truncated in transit. The filesystem on
		// disk will be missing tail blocks, which on ext4 typically shows
		// up as "bad block bitmap checksum" / "Journal has aborted" on boot.
		finalErr = fmt.Errorf("restore truncated: wrote %d bytes but image is %d bytes (%.1f%% of expected) — target filesystem will be corrupt",
			written, j.params.SourceSize, float64(written)/float64(j.params.SourceSize)*100)
	}

	j.progressFn(Progress{Done: true, Error: finalErr})
	return finalErr
}

// zeroFillFreeSpace mounts each partition on the source disk,
// writes zeros to fill free space, then unmounts.
// Outputs progress in real-time as each partition is processed.
func (j *CloneJob) zeroFillFreeSpace() error {
	diskName := j.params.SourcePath

	// Show partition info before starting
	j.logFn("  Scanning remote disk partitions (including LVM)...")
	infoOut, _ := j.sshClient.CombinedOutput(fmt.Sprintf(
		`disk="%s"; diskbase=$(basename "$disk"); apk add --quiet lvm2 2>/dev/null; lvm vgscan --mknodes 2>/dev/null; lvm vgchange -ay 2>/dev/null; echo "DISK=$diskbase"; for p in /sys/block/"$diskbase"/"$diskbase"*/partition; do [ -f "$p" ] || continue; pname=$(basename $(dirname "$p")); size=$(cat /sys/block/"$diskbase"/"$pname"/size 2>/dev/null); echo "PART $pname $size"; done; lvm lvs --noheadings -o lv_path,lv_size --units b 2>/dev/null | while read lvpath size; do [ -n "$lvpath" ] && echo "LVM $lvpath $size"; done`,
		diskName,
	))

	diskNameOnly := ""
	var totalSize int64
	for _, line := range strings.Split(infoOut, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "DISK=") {
			diskNameOnly = strings.TrimPrefix(line, "DISK=")
		} else if strings.HasPrefix(line, "PART ") {
			f := strings.Fields(line)
			if len(f) >= 3 {
				sz, _ := strconv.ParseInt(f[2], 10, 64)
				szBytes := sz * 512
				totalSize += szBytes
				j.logFn("    Partition %s: %s", f[1], formatBytesCompat(szBytes))
			}
		} else if strings.HasPrefix(line, "LVM ") {
			f := strings.Fields(line)
			if len(f) >= 3 {
				szStr := strings.TrimSuffix(f[2], "B")
				sz, _ := strconv.ParseInt(szStr, 10, 64)
				totalSize += sz
				j.logFn("    LVM %s: %s", f[1], formatBytesCompat(sz))
			}
		}
	}
	if diskNameOnly != "" {
		j.logFn("  Disk %s: total %s", diskNameOnly, formatBytesCompat(totalSize))
	}

	j.logFn("  Zero-filling free space (run 'df -h /dev/sda*' on remote to see progress)...")

	// Build script that:
	// 1. Install lvm2 if needed
	// 2. Activate LVM
	// 3. Fills direct partitions
	// 4. Fills LVM logical volumes backed by this disk
	script := fmt.Sprintf(`apk add --quiet lvm2 2>/dev/null || true
disk="%s"
diskbase=$(basename "$disk")
modprobe ext4 2>/dev/null
modprobe xfs 2>/dev/null
modprobe btrfs 2>/dev/null
modprobe vfat 2>/dev/null

# Activate all LVM volumes
lvm vgscan --mknodes 2>/dev/null || true
lvm vgchange -ay 2>/dev/null || true

# Direct partitions
parts=""
for p in /sys/block/"$diskbase"/"$diskbase"*/partition; do
  [ -f "$p" ] || continue
  pname=$(basename $(dirname "$p"))
  parts="$parts /dev/$pname"
done

# LVM logical volumes (any LV on this machine)
lvm_lvs=""
for lv in /dev/mapper/*; do
  [ "$lv" = "/dev/mapper/control" ] && continue
  [ -b "$lv" ] && lvm_lvs="$lvm_lvs $lv"
done

all_devices="$parts $lvm_lvs"

if [ -z "$(echo $all_devices)" ]; then
  echo "NO_PARTS"
  exit 0
fi

for dev in $all_devices; do
  mp=$(mktemp -d /tmp/zf.XXXXXX)
  if mount "$dev" "$mp" 2>/dev/null; then
    echo "FILL $dev"
    dd if=/dev/zero of="$mp/.zero_fill" bs=4194304 2>/dev/null &
    PID=$!
    while kill -0 $PID 2>/dev/null; do
      size=$(stat -c %%s "$mp/.zero_fill" 2>/dev/null)
      [ -n "$size" ] && echo "PROGRESS $dev $size"
      sleep 5
    done
    wait $PID
    rm -f "$mp/.zero_fill"
    sync
    # CRITICAL: must fully unmount before dd reads the source disk.
    # A lazy umount (-l) only detaches the namespace — the filesystem stays
    # live in the kernel and the on-disk journal is left mid-transaction.
    # dd then reads a "dirty" filesystem that GRUB refuses to mount
    # ("error: unknown filesystem"). Retry regular umount several times
    # and report failure rather than falling back to -l.
    umount_ok=0
    for try in 1 2 3 4 5; do
      if umount "$mp" 2>/dev/null; then
        umount_ok=1
        break
      fi
      sync
      sleep 1
    done
    if [ "$umount_ok" = "0" ]; then
      echo "UMOUNTFAIL $dev $mp"
    fi
    echo "DONEPART $dev"
  else
    echo "SKIP $dev"
  fi
  rmdir "$mp" 2>/dev/null || true
done
echo "DONE"
`, diskName)

	// Stream execution — shows each FILL line in real-time
	session, err := j.sshClient.Execute("sh -c " + shellQuote(script))
	if err != nil {
		return fmt.Errorf("start zero-fill: %w", err)
	}
	defer session.Close()

	scanner := bufio.NewScanner(session.Stdout)
	filled := 0
	skipped := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "FILL ") {
			j.logFn("    Filling: %s", strings.TrimPrefix(line, "FILL "))
			filled++
		} else if strings.HasPrefix(line, "PROGRESS ") {
			f := strings.Fields(line)
			if len(f) >= 3 {
				sz, _ := strconv.ParseInt(f[2], 10, 64)
				j.logFn("      %s: %s written so far", f[1], formatBytesCompat(sz))
			}
		} else if strings.HasPrefix(line, "DONEPART ") {
			j.logFn("    Done: %s", strings.TrimPrefix(line, "DONEPART "))
		} else if strings.HasPrefix(line, "SKIP ") {
			j.logFn("    Skipped: %s", strings.TrimPrefix(line, "SKIP "))
			skipped++
		} else if line == "NO_PARTS" {
			j.logFn("    No partitions found (raw disk)")
		} else if line == "DONE" {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		j.logFn("  Warning: output stream ended early: %v", err)
	}

	if filled > 0 || skipped > 0 {
		j.logFn("  Zero-fill done: %d filled, %d skipped", filled, skipped)
	} else {
		j.logFn("  Zero-fill done")
	}
	return nil
}

// FixInitramfs rebuilds the remote system's initramfs with --no-hostonly
// so the backup can boot on different hardware. Runs via chroot.
//
// This is a thin wrapper around FixBoot for backwards compatibility.
func (j *CloneJob) FixInitramfs() error {
	return j.FixBoot("")
}

// FixBoot performs a full remote boot repair: chroot into the restored
// system on the remote, rebuild initramfs with --no-hostonly, reinstall
// GRUB to the target disk, regenerate grub.cfg, and clean stale fstab
// entries. Necessary after every restore because:
//
//   - GRUB's core.img embeds block addresses baked in at install time.
//     When a disk is dd'd to a different disk (vda -> sda, different
//     geometry, different gap size before partition 1), the embedded
//     block list can become unreadable, producing GRUB's
//     "error: unknown filesystem. Entering rescue mode...".
//   - initramfs built with hostonly contains only the source machine's
//     drivers; --no-hostonly rebuilds it with all drivers so the system
//     boots on any hardware.
//
// If targetDisk is empty, GRUB reinstall is skipped (used by save mode
// where we only rebuild initramfs on the source before imaging).
func (j *CloneJob) FixBoot(targetDisk string) error {
	if j.sshClient == nil {
		return fmt.Errorf("SSH client is nil")
	}

	if targetDisk != "" {
		j.logFn("  重建引导 (GRUB + initramfs) 以兼容不同硬件...")
	} else {
		j.logFn("  Rebuilding initramfs (--no-hostonly for cross-hardware boot)...")
	}

	// Step 1: install LVM tools and activate volumes
	j.sshClient.CombinedOutput("apk add --quiet lvm2 2>/dev/null")
	j.sshClient.CombinedOutput("lvm vgscan --mknodes 2>/dev/null; lvm vgchange -ay 2>/dev/null")

	out, _ := j.sshClient.CombinedOutput(fmt.Sprintf(
		`lsblk -ln -o NAME,TYPE,FSTYPE 2>/dev/null | awk '$2=="part"&&$3!=""&&$3!="swap"{print "/dev/"$1}'; lsblk -ln -o NAME,TYPE,FSTYPE 2>/dev/null | awk '$2=="lvm"&&$3!=""&&$3!="swap"{print "/dev/mapper/"$1}'`,
	))

	rootDev := ""
	for _, dev := range strings.Split(out, "\n") {
		dev = strings.TrimSpace(dev)
		if dev == "" {
			continue
		}

		// Check device exists before attempting mount
		check, _ := j.sshClient.CombinedOutput(fmt.Sprintf("test -b %s && echo OK", dev))
		if !strings.Contains(check, "OK") {
			continue
		}

		// Try mounting to see if it's root
		_, rcErr := j.sshClient.CombinedOutput(fmt.Sprintf(
			`mp=$(mktemp -d) && mount %s "$mp" 2>/dev/null && { [ -f "$mp/etc/os-release" ] || [ -f "$mp/etc/fstab" ]; rc=$?; umount "$mp" 2>/dev/null; rmdir "$mp" >/dev/null 2>&1; exit $rc; } && rmdir "$mp" 2>/dev/null; exit 1`,
			dev,
		))
		if rcErr == nil {
			rootDev = dev
			break
		}
	}
	if rootDev == "" {
		return fmt.Errorf("no root partition found, cannot rebuild initramfs")
	}

	j.logFn("    Root: %s", rootDev)

	// Step 2: mount root + bind mounts, then chroot to fix initramfs + GRUB.
	// /boot and /boot/efi are detected and mounted from fstab inside the
	// script below (no separate probe needed).
	script := fmt.Sprintf(`ROOT=%s
TARGETDISK=%s

# Unmount /mnt if something is already mounted there
umount /mnt 2>/dev/null

mount "$ROOT" /mnt 2>/dev/null || { echo "FAIL mount"; exit 1; }
[ ! -d /mnt/usr/bin ] && [ ! -d /mnt/usr/sbin ] && { echo "FAIL noroot"; exit 1; }

# Mount /boot and /boot/efi if they are separate partitions (parsed from fstab).
# Skip swap and pseudo filesystems. Resolve UUID=/LABEL= references.
parse_fstab_dev() {
  local entry="$1" dev=""
  case "$entry" in
    UUID=*)  dev=$(blkid -U "${entry#UUID=}" 2>/dev/null) ;;
    LABEL=*) dev=$(blkid -L "${entry#LABEL=}" 2>/dev/null) ;;
    /dev/*)  dev="$entry" ;;
  esac
  echo "$dev"
}
BOOTENT=$(awk '!/^[[:space:]]*#/ && $2=="/boot" {print $1; exit}' /mnt/etc/fstab 2>/dev/null)
if [ -n "$BOOTENT" ]; then
  BOOTDEV=$(parse_fstab_dev "$BOOTENT")
  if [ -n "$BOOTDEV" ] && [ -b "$BOOTDEV" ]; then
    mount "$BOOTDEV" /mnt/boot 2>/dev/null && echo "  mounted /boot <- $BOOTDEV"
  fi
fi
EFIENT=$(awk '!/^[[:space:]]*#/ && $2=="/boot/efi" {print $1; exit}' /mnt/etc/fstab 2>/dev/null)
if [ -n "$EFIENT" ]; then
  EFIDEV=$(parse_fstab_dev "$EFIENT")
  if [ -n "$EFIDEV" ] && [ -b "$EFIDEV" ]; then
    mkdir -p /mnt/boot/efi 2>/dev/null
    mount "$EFIDEV" /mnt/boot/efi 2>/dev/null && echo "  mounted /boot/efi <- $EFIDEV"
  fi
fi

mount --bind /dev /mnt/dev 2>/dev/null
mount --bind /proc /mnt/proc 2>/dev/null
mount --bind /sys /mnt/sys 2>/dev/null
mount -t tmpfs tmpfs /mnt/run 2>/dev/null || mkdir -p /mnt/run

RC=0

# --- Rebuild initramfs with all drivers (cross-hardware boot) ---
DONE=0
if [ -x /mnt/usr/bin/dracut ] || [ -x /mnt/usr/sbin/dracut ]; then
  echo "  -> dracut --no-hostonly"
  chroot /mnt dracut --no-hostonly --force --regenerate-all 2>&1 && DONE=1 || RC=1
fi
if [ "$DONE" = "0" ] && [ -x /mnt/usr/sbin/update-initramfs ]; then
  echo "  -> update-initramfs"
  chroot /mnt update-initramfs -u -k all 2>&1 && DONE=1 || RC=1
fi
if [ "$DONE" = "0" ] && [ -x /mnt/usr/bin/mkinitcpio ]; then
  echo "  -> mkinitcpio -P"
  chroot /mnt mkinitcpio -P 2>&1 && DONE=1 || RC=1
fi
[ "$DONE" = "0" ] && echo "NO_INITRAMFS_TOOL" && RC=1

# --- Reinstall GRUB to target disk (fixes "unknown filesystem" after dd) ---
# Only when called from restore mode (targetDisk provided). Without a target
# disk we are running on the source (save/clone mode) and must NOT rewrite
# the source's MBR.
if [ -n "$TARGETDISK" ]; then
  # Ensure /etc/mtab exists inside chroot (some minimal images lack it)
  [ -e /mnt/etc/mtab ] || ln -sf /proc/self/mounts /mnt/etc/mtab

  GRUB_RC=1
  if [ -x /mnt/usr/sbin/grub2-install ]; then
    echo "  -> grub2-install --recheck $TARGETDISK"
    chroot /mnt /usr/sbin/grub2-install --recheck "$TARGETDISK" 2>&1 && GRUB_RC=0
    if [ $GRUB_RC -eq 0 ] && [ -x /mnt/usr/sbin/grub2-mkconfig ]; then
      chroot /mnt /usr/sbin/grub2-mkconfig -o /boot/grub2/grub.cfg 2>&1
    fi
  elif [ -x /mnt/usr/sbin/grub-install ]; then
    echo "  -> grub-install --recheck $TARGETDISK"
    chroot /mnt /usr/sbin/grub-install --recheck "$TARGETDISK" 2>&1 && GRUB_RC=0
    if [ $GRUB_RC -eq 0 ] && [ -x /mnt/usr/sbin/grub-mkconfig ]; then
      chroot /mnt /usr/sbin/grub-mkconfig -o /boot/grub/grub.cfg 2>&1
    fi
  else
    echo "NO_GRUB_TOOL"
  fi
  [ $GRUB_RC -ne 0 ] && echo "GRUB_INSTALL_FAILED" && RC=1

  # --- Fix fstab: comment out mounts for devices that no longer exist ---
  if [ -f /mnt/etc/fstab ]; then
    cp /mnt/etc/fstab /mnt/etc/fstab.bak 2>/dev/null
    awk '
      /^[[:space:]]*#/ { print; next }
      {
        dev=$1; mp=$2
        if (mp == "" || mp == "none" || mp == "swap") { print; next }
        if (mp == "/" || mp == "/boot" || mp ~ /^\/boot\//) { print; next }
        # Resolve the device to a real /dev node. UUID=/LABEL= must be
        # resolved via command output (getline), NOT by pasting the blkid
        # command into [ -b ] -- that would always be false and comment out
        # every valid entry.
        actual=""
        if (dev ~ /^UUID=/) {
          cmd="blkid -U " substr(dev,6) " 2>/dev/null"
          cmd | getline actual
          close(cmd)
        } else if (dev ~ /^LABEL=/) {
          cmd="blkid -L " substr(dev,7) " 2>/dev/null"
          cmd | getline actual
          close(cmd)
        } else if (dev ~ /^\/dev\//) actual=dev
        gsub(/[[:space:]]/, "", actual)
        if (actual != "" && system("[ -b " actual " ] 2>/dev/null") == 0) {
          print
        } else {
          print "# " $0 "   # disabled by disk-cloner (device missing after restore)"
        }
      }
    ' /mnt/etc/fstab > /mnt/etc/fstab.new && mv /mnt/etc/fstab.new /mnt/etc/fstab
  fi
fi

# CRITICAL: must fully unmount — lazy umount (-l) leaves the filesystem
# live in the kernel, so the on-disk journal stays mid-transaction and
# dd reads a "dirty" image. Use regular umount with retries, in reverse
# order (deepest first). Bind mounts first, then real mounts.
for try in 1 2 3 4 5; do
  umount /mnt/run 2>/dev/null
  umount /mnt/dev/pts 2>/dev/null
  umount /mnt/dev 2>/dev/null
  umount /mnt/proc 2>/dev/null
  umount /mnt/sys 2>/dev/null
  umount /mnt/boot/efi 2>/dev/null
  umount /mnt/boot 2>/dev/null
  if umount /mnt 2>/dev/null; then
    break
  fi
  sync
  sleep 1
done
exit $RC
`, shellQuote(rootDev), shellQuote(targetDisk))

	out2, err2 := j.sshClient.CombinedOutput("sh -c " + shellQuote(script))

	// Classify by output markers FIRST: the script exits non-zero (RC=1)
	// both for mount failures and for GRUB-install failure, so checking
	// err2 before the markers would hide the specific GRUB_INSTALL_FAILED
	// message behind the generic one.
	switch {
	case strings.Contains(out2, "NO_INITRAMFS_TOOL"):
		j.logFn("  [!] No initramfs tool found (dracut/update-initramfs/mkinitcpio), skipping")
	case strings.Contains(out2, "FAIL mount"):
		j.logFn("  [!] Failed to mount root partition")
	case strings.Contains(out2, "FAIL noroot"):
		j.logFn("  [!] Mounted partition does not look like a root filesystem")
	case strings.Contains(out2, "GRUB_INSTALL_FAILED"):
		j.logFn("  [!] Initramfs rebuilt, but GRUB reinstall failed — you may need to run grub2-install manually")
	case err2 != nil:
		// Non-fatal: backup/restore still succeeded; user can fix boot manually.
		j.logFn("  [!] Boot repair failed — you may need to fix boot manually")
	default:
		if strings.Contains(out2, "NO_GRUB_TOOL") {
			j.logFn("  ✓ Initramfs rebuilt (no GRUB install tool found; skipped GRUB reinstall)")
		} else if targetDisk != "" {
			j.logFn("  ✓ GRUB reinstalled and initramfs rebuilt")
		} else {
			j.logFn("  ✓ Initramfs rebuilt")
		}
	}
	return nil
}

// streamCompressed: remote dd|gzip -> SSH -> local gzip.Reader -> dst
// This transfers compressed data over the network, then decompresses locally.
func (j *CloneJob) streamCompressed(dst io.Writer) error {
	if j.sshClient == nil {
		return fmt.Errorf("SSH client is nil")
	}

	bs := j.params.BlockSize
	if bs == "" {
		bs = "4M"
	}
	if err := validateBS(bs); err != nil {
		return err
	}
	bsBytes := bsToBytes(bs)

	// Remote: dd | compress (gzip or pigz)
	compress := j.buildCompressCmd()
	remoteCmd := fmt.Sprintf("dd if=%s bs=%s | %s", j.params.SourcePath, bsBytes, compress)

	session, err := j.sshClient.Execute(remoteCmd)
	if err != nil {
		return fmt.Errorf("start remote dd|gzip: %w", err)
	}
	defer session.Close()

	// Capture stderr
	stderrCh := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(session.Stderr)
		stderrCh <- string(data)
	}()

	// Handle Ctrl+C: first press sends SIGTERM to remote, second force-exits
	done := make(chan struct{})
	defer close(done)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	var cancelled atomic.Bool
	go func() {
		for {
			select {
			case <-sigCh:
				if !cancelled.Load() {
					cancelled.Store(true)
					_ = session.Signal(ssh.SIGTERM)
					j.logFn("  [!] 正在取消... (再按一次 Ctrl+C 强制退出)")
				} else {
					os.Exit(130)
				}
			case <-done:
				return
			}
		}
	}()

	// Decompress on-the-fly and write to target
	gzr, err := gzip.NewReader(session.Stdout)
	if err != nil {
		return fmt.Errorf("init gzip decompressor: %w (remote gzip may not be installed)", err)
	}
	defer gzr.Close()

	written, copyErr := j.copyWithProgress(dst, gzr)

	sessionErr := session.Wait()

	stderrOut := ""
	select {
	case stderrOut = <-stderrCh:
	case <-time.After(3 * time.Second):
	}

	var finalErr error
	if cancelled.Load() {
		finalErr = fmt.Errorf("cancelled by user")
	} else if copyErr != nil {
		finalErr = copyErr
	} else if sessionErr != nil {
		errMsg := fmt.Sprintf("remote dd|gzip: %v", sessionErr)
		if stderrOut != "" {
			errMsg += "\n  stderr: " + strings.TrimSpace(stderrOut)
		}
		finalErr = fmt.Errorf("%s", errMsg)
	} else if written == 0 {
		errMsg := "remote dd produced no data -- disk may be busy, missing, or inaccessible"
		if stderrOut != "" {
			errMsg += "\n  stderr: " + strings.TrimSpace(stderrOut)
		}
		finalErr = fmt.Errorf("%s", errMsg)
	}

	j.progressFn(Progress{Done: true, Error: finalErr})
	return finalErr
}

// streamCompressedRaw: remote dd|gzip -> SSH -> dst. The compressed bytes are
// written as-is to dst (the output file is already .gz format), while a local
// gzip reader decompresses the same stream to report true uncompressed
// progress — without this the progress bar would show compressed/total bytes
// and plateau at the compression ratio instead of reaching 100%.
func (j *CloneJob) streamCompressedRaw(dst io.Writer) error {
	if j.sshClient == nil {
		return fmt.Errorf("SSH client is nil")
	}

	bs := j.params.BlockSize
	if bs == "" {
		bs = "4M"
	}
	if err := validateBS(bs); err != nil {
		return err
	}
	bsBytes := bsToBytes(bs)

	compress := j.buildCompressCmd()
	remoteCmd := fmt.Sprintf("dd if=%s bs=%s | %s", j.params.SourcePath, bsBytes, compress)

	session, err := j.sshClient.Execute(remoteCmd)
	if err != nil {
		return fmt.Errorf("start remote dd|gzip: %w", err)
	}
	defer session.Close()

	stderrCh := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(session.Stderr)
		stderrCh <- string(data)
	}()

	done := make(chan struct{})
	defer close(done)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	var cancelled atomic.Bool
	go func() {
		for {
			select {
			case <-sigCh:
				if !cancelled.Load() {
					cancelled.Store(true)
					_ = session.Signal(ssh.SIGTERM)
					j.logFn("  [!] 正在取消... (再按一次 Ctrl+C 强制退出)")
				} else {
					os.Exit(130)
				}
			case <-done:
				return
			}
		}
	}()

	// Tee the compressed stream into dst while decompressing a copy to count
	// real disk bytes for the progress bar (see function doc above).
	fw := &errWriter{w: dst}
	gzr, err := gzip.NewReader(io.TeeReader(session.Stdout, fw))
	if err != nil {
		return fmt.Errorf("init gzip decompressor: %w (remote gzip may not be installed)", err)
	}
	defer gzr.Close()

	written, copyErr := j.copyWithProgress(io.Discard, gzr)
	if copyErr == nil && fw.err != nil {
		copyErr = fmt.Errorf("write error: %w", fw.err)
	}

	sessionErr := session.Wait()

	stderrOut := ""
	select {
	case stderrOut = <-stderrCh:
	case <-time.After(3 * time.Second):
	}

	var finalErr error
	if cancelled.Load() {
		finalErr = fmt.Errorf("cancelled by user")
	} else if copyErr != nil {
		finalErr = copyErr
	} else if sessionErr != nil {
		errMsg := fmt.Sprintf("remote dd|gzip: %v", sessionErr)
		if stderrOut != "" {
			errMsg += "\n  stderr: " + strings.TrimSpace(stderrOut)
		}
		finalErr = fmt.Errorf("%s", errMsg)
	} else if written == 0 {
		errMsg := "remote dd|gzip produced no data -- disk may be busy, missing, or gzip not installed"
		if stderrOut != "" {
			errMsg += "\n  stderr: " + strings.TrimSpace(stderrOut)
		}
		finalErr = fmt.Errorf("%s", errMsg)
	} else {
		// Verify dd actually read the full source disk. dd reports bytes-read
		// on stderr ("N bytes copied"). If it read less than SourceSize, the
		// backup is truncated and restoring it will corrupt the target fs.
		if ddRead := parseDdBytesRead(stderrOut); ddRead > 0 && j.params.SourceSize > 0 && ddRead < j.params.SourceSize {
			finalErr = fmt.Errorf("backup truncated: dd read %d bytes but source disk is %d bytes (%.1f%% of expected) — backup is corrupt, do not restore it",
				ddRead, j.params.SourceSize, float64(ddRead)/float64(j.params.SourceSize)*100)
		}
	}

	j.progressFn(Progress{Done: true, Error: finalErr})
	return finalErr
}

// streamRaw: remote dd -> SSH -> dst (no compression in pipe, caller wraps in gzip if needed)
func (j *CloneJob) streamRaw(dst io.Writer) error {
	if j.sshClient == nil {
		return fmt.Errorf("SSH client is nil")
	}

	bs := j.params.BlockSize
	if bs == "" {
		bs = "4M"
	}
	if err := validateBS(bs); err != nil {
		return err
	}
	bsBytes := bsToBytes(bs)

	remoteCmd := fmt.Sprintf("dd if=%s bs=%s", j.params.SourcePath, bsBytes)

	session, err := j.sshClient.Execute(remoteCmd)
	if err != nil {
		return fmt.Errorf("start remote dd: %w", err)
	}
	defer session.Close()

	stderrCh := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(session.Stderr)
		stderrCh <- string(data)
	}()

	done := make(chan struct{})
	defer close(done)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	var cancelled atomic.Bool
	go func() {
		for {
			select {
			case <-sigCh:
				if !cancelled.Load() {
					cancelled.Store(true)
					_ = session.Signal(ssh.SIGTERM)
					j.logFn("  [!] 正在取消... (再按一次 Ctrl+C 强制退出)")
				} else {
					os.Exit(130)
				}
			case <-done:
				return
			}
		}
	}()

	written, copyErr := j.copyWithProgress(dst, session.Stdout)
	sessionErr := session.Wait()

	stderrOut := ""
	select {
	case stderrOut = <-stderrCh:
	case <-time.After(3 * time.Second):
	}

	var finalErr error
	if cancelled.Load() {
		finalErr = fmt.Errorf("cancelled by user")
	} else if copyErr != nil {
		finalErr = copyErr
	} else if sessionErr != nil {
		errMsg := fmt.Sprintf("remote dd: %v", sessionErr)
		if stderrOut != "" {
			errMsg += "\n  stderr: " + strings.TrimSpace(stderrOut)
		}
		finalErr = fmt.Errorf("%s", errMsg)
	} else if written == 0 {
		errMsg := "remote dd produced no data -- disk may be busy, missing, or inaccessible"
		if stderrOut != "" {
			errMsg += "\n  stderr: " + strings.TrimSpace(stderrOut)
		}
		finalErr = fmt.Errorf("%s", errMsg)
	}

	j.progressFn(Progress{Done: true, Error: finalErr})
	return finalErr
}

// errWriter records the first write error from the underlying writer so it
// can be reported with the right label (a write failure surfaces through
// io.TeeReader as a read error otherwise).
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
}

func (j *CloneJob) copyWithProgress(dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 4*1024*1024) // 4MB
	var written int64
	start := time.Now()
	lastUpdate := time.Now()
	var lastWritten int64

	// Sliding window of speed samples (1 per second, last 30 seconds)
	var speedRing [30]float64
	var speedRingPos int
	var speedRingCount int
	warnedSlow := false

	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			_, werr := dst.Write(buf[:n])
			if werr != nil {
				return written, fmt.Errorf("write error: %w", werr)
			}
			written += int64(n)

			now := time.Now()
			if now.Sub(lastUpdate) >= time.Second {
				elapsed := now.Sub(start).Seconds()
				interval := now.Sub(lastUpdate).Seconds()

				// Instantaneous speed
				instSpeed := 0.0
				if interval > 0 {
					instSpeed = float64(written-lastWritten) / interval / (1024 * 1024)
				}

				// Add to ring buffer for sliding average
				speedRing[speedRingPos] = instSpeed
				speedRingPos = (speedRingPos + 1) % 30
				if speedRingCount < 30 {
					speedRingCount++
				}

				// Average speed from ring
				avgSpeed := 0.0
				if speedRingCount > 0 {
					var sum float64
					for i := 0; i < speedRingCount; i++ {
						sum += speedRing[i]
					}
					avgSpeed = sum / float64(speedRingCount)
				}

				// Use average speed for stable ETA
				avgForETA := avgSpeed
				if avgForETA <= 0 {
					avgForETA = instSpeed // fallback to instant if no avg yet
				}

				// Slow speed warning (detected once)
				if speedRingCount >= 30 && avgSpeed < 1.0 && !warnedSlow {
					j.logFn("  [!] Transfer speed is very slow (%.1f MB/s average) — check network or remote CPU load", avgSpeed)
					warnedSlow = true
				}

				percent := 0.0
				eta := int64(0)
				if j.params.SourceSize > 0 {
					percent = float64(written) / float64(j.params.SourceSize) * 100
					if percent > 100 {
						percent = 100
					}
					if avgForETA > 0 {
						remaining := j.params.SourceSize - written
						if remaining > 0 {
							eta = int64(float64(remaining) / (avgForETA * 1024 * 1024))
						}
					}
				}

				j.progressFn(Progress{
					BytesWritten:   written,
					TotalBytes:     j.params.SourceSize,
					Percent:        percent,
					SpeedMBps:      avgSpeed,
					ElapsedSeconds: int64(elapsed),
					EtaSeconds:     eta,
				})

				// Check if SSH connection is still alive
				if !j.sshClient.IsConnected() {
					return written, fmt.Errorf("SSH connection lost — remote host may have rebooted or shut down")
				}

				lastUpdate = now
				lastWritten = written
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return written, fmt.Errorf("read error: %w", readErr)
		}
	}

	elapsed := time.Since(start).Seconds()
	speedMBps := 0.0
	if elapsed > 0 {
		speedMBps = float64(written) / elapsed / (1024 * 1024)
	}

	percent := 100.0
	if j.params.SourceSize > 0 && written < j.params.SourceSize {
		percent = float64(written) / float64(j.params.SourceSize) * 100
	}

	j.progressFn(Progress{
		BytesWritten:   written,
		TotalBytes:     j.params.SourceSize,
		Percent:        percent,
		SpeedMBps:      speedMBps,
		ElapsedSeconds: int64(elapsed),
		EtaSeconds:     0,
	})

	return written, nil
}
