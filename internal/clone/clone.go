package clone

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
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
	runner     sshclient.Runner
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

func New(runner sshclient.Runner, params Params, progressFn func(Progress)) *CloneJob {
	return &CloneJob{
		runner:     runner,
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

// ValidateBlockSize exports the block-size safety check so callers can
// validate user input before any long-running pre-transfer step
// (zero-fill, initramfs rebuild) — a bad block size must fail fast.
func ValidateBlockSize(bs string) error {
	return validateBS(bs)
}

var safeBSRe = regexp.MustCompile(`^[0-9]+[KMGkmg]?$`)

func validateBS(bs string) error {
	if !safeBSRe.MatchString(bs) {
		return fmt.Errorf("invalid block size: %q", bs)
	}
	numStr := strings.TrimRight(bs, "KMGkmg")
	n, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid block size: %q (must be > 0)", bs)
	}
	mult := int64(1)
	switch unicode.ToUpper(rune(bs[len(bs)-1])) {
	case 'K':
		mult = 1024
	case 'M':
		mult = 1024 * 1024
	case 'G':
		mult = 1024 * 1024 * 1024
	}
	if n > math.MaxInt64/mult {
		return fmt.Errorf("invalid block size: %q overflows the byte counter", bs)
	}
	return nil
}

// bsToBytes converts human-readable block size to bytes string.
// Busybox dd does NOT support suffixes like "4M" — only plain byte counts.
// Callers must validateBS first; this function assumes a validated input
// and falls back to the 4 MiB default on anything unexpected.
func bsToBytes(bs string) string {
	if err := validateBS(bs); err != nil {
		return "4194304"
	}
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
// Returns 0 if unknown (image ≥ 4 GiB without a .size sidecar — the gzip
// ISIZE footer wraps modulo 2^32 above 4 GiB and cannot be disambiguated
// from the compressed file alone). Prefer ReadSizeFile when a sidecar
// exists.
func GzipUncompressedSize(path string) int64 {
	return readGzipISize(path)
}

// gzipMaxCompressRatio is deflate's worst-case expansion ratio (~1032:1
// for long zero runs). If even a maximally-compressible image of 4 GiB
// would produce a compressed file larger than the one we are reading, the
// true uncompressed size is provably below 4 GiB and the footer is exact.
const gzipMaxCompressRatio = 1032

// readGzipISize reads the last 4 bytes of a gzip file to get the
// uncompressed data size (ISIZE, stored as uint32 LE modulo 2^32).
//
// The footer is only exact for images < 4 GiB; above that it wraps modulo
// 2^32 and the compressed file alone carries no way to recover the true
// size (a zero-filled 6 GiB disk and a 2 GiB disk can produce byte-identical
// footers vs. sizes). Guessing here made restore show wrong progress and
// report false "truncated restore" errors, so this returns 0 ("unknown")
// whenever wraparound is possible. Callers should prefer the exact .size
// sidecar written at save time (ReadSizeFile).
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

	info, err := f.Stat()
	if err != nil || info.Size() <= 0 {
		return 0
	}
	// true size ≤ gzipMaxCompressRatio × compressed; when even that upper
	// bound stays under 4 GiB the footer cannot have wrapped.
	if info.Size() < (4<<30)/gzipMaxCompressRatio {
		return size
	}
	return 0 // likely (or possibly) wrapped — unknown
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

// sizeFileSuffix is appended to the image path for the sidecar file that
// records the exact uncompressed size. gzip's ISIZE footer wraps modulo 2^32
// above 4 GiB, so for larger images the sidecar written at save time is the
// only exact source for the uncompressed size.
const sizeFileSuffix = ".size"

// WriteSizeFile records the uncompressed image size in a "<path>.size"
// sidecar file. Called after a successful save. A failed write only
// degrades the >4 GiB truncation pre-check on restore (the gzip ISIZE
// footer wraps above 4 GiB), so callers should warn, not fail the save.
func WriteSizeFile(path string, size int64) error {
	return os.WriteFile(path+sizeFileSuffix, []byte(strconv.FormatInt(size, 10)+"\n"), 0644)
}

// ReadSizeFile returns the uncompressed size recorded by WriteSizeFile,
// or 0 if the sidecar is missing or unparsable.
func ReadSizeFile(path string) int64 {
	data, err := os.ReadFile(path + sizeFileSuffix)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
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
	_, err := j.runner.CombinedOutput("command -v " + cmd)
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
	out, err := j.runner.CombinedOutput("dd if=/dev/zero of=/dev/null bs=1 count=1 conv=fdatasync 2>&1")
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

// parseDdRecordsOut extracts the count of full output blocks from dd's
// "X+Y records out" stderr line. BusyBox dd never prints the GNU
// "N bytes ... copied" summary, and that summary is locale-dependent —
// "X+Y records out" stays English, so it is the locale-independent
// fallback whenever parseDdBytesRead finds nothing. Returns -1 when no
// usable line exists.
func parseDdRecordsOut(stderr string) int64 {
	res := int64(-1)
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasSuffix(line, " records out") {
			continue
		}
		f := strings.Fields(strings.TrimSuffix(line, " records out"))
		if len(f) != 1 || !strings.Contains(f[0], "+") {
			continue
		}
		parts := strings.SplitN(f[0], "+", 2)
		if n, err := strconv.ParseInt(parts[0], 10, 64); err == nil && n >= 0 {
			res = n
		}
	}
	return res
}

// ddRecordsTruncated reports whether a full-block count from
// parseDdRecordsOut proves dd stopped short of sourceSize: a complete read
// of sourceSize bytes at block size bs ends with X*bs+bs > sourceSize, so
// the converse means truncation. X*bs is only a lower bound on the bytes
// moved, so a truncation smaller than bs can go undetected. Returns false
// whenever the inputs cannot support a verdict.
func ddRecordsTruncated(recs, bs, sourceSize int64) bool {
	if recs < 0 || bs <= 0 || sourceSize <= 0 {
		return false
	}
	if recs > sourceSize/bs {
		return false // already at/over the disk size — no proof (and no overflow)
	}
	return recs*bs+bs <= sourceSize
}

// mountedSourcePartitions lists the mount points of all partitions (and
// dm/LV devices) belonging to the source disk, parsed locally from
// /proc/mounts so the matching logic is testable and injection-safe.
func (j *CloneJob) mountedSourcePartitions(src string) ([]string, error) {
	diskBase := src
	if i := strings.LastIndex(src, "/"); i >= 0 {
		diskBase = src[i+1:]
	}
	out, err := j.runner.CombinedOutput("cat /proc/mounts 2>/dev/null")
	if err != nil {
		// Fail closed: treating an unreadable mount table as "nothing
		// mounted" would silently pass the pre-dd abort gates.
		return nil, fmt.Errorf("read /proc/mounts on remote: %w", err)
	}
	// LVM logical volumes appear in /proc/mounts as /dev/mapper/<vg>-<lv>
	// or /dev/dm-N — names that bear no relation to the source disk name,
	// so they can only be attributed via the kernel's slave topology.
	lvmDevs := j.lvmDevicesOnDisk(src)
	var mounted []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if devMatchesSourceDisk(fields[0], diskBase) || lvmDevs[fields[0]] {
			mounted = append(mounted, fields[1])
		}
	}
	return mounted, nil
}

// lvmDevicesOnDisk returns the set of device paths of device-mapper volumes
// that physically live on the source disk — a dm device counts when any of
// its /sys/block/dm-N/slaves is the disk or one of its partitions. Without
// this, a still-mounted LVM root (/dev/mapper/vg-root) was invisible to the
// pre-dd "must abort" check and dd produced a torn image of a live FS.
func (j *CloneJob) lvmDevicesOnDisk(src string) map[string]bool {
	set := map[string]bool{}
	diskBase := src
	if i := strings.LastIndex(src, "/"); i >= 0 {
		diskBase = src[i+1:]
	}
	script := fmt.Sprintf(`diskbase=%s
for d in /sys/block/dm-*; do
  [ -d "$d/slaves" ] || continue
  owned=0
  for s in "$d"/slaves/*; do
    sname=${s##*/}
    case "$sname" in
      "$diskbase"|"$diskbase"p[0-9]*|"$diskbase"[0-9]*) owned=1 ;;
    esac
  done
  [ "$owned" = "1" ] || continue
  name=$(cat "$d/dm/name" 2>/dev/null)
  [ -n "$name" ] && echo "/dev/mapper/$name"
  echo "/dev/${d##*/}"
done`, shellQuote(diskBase))
	out, _ := j.runner.CombinedOutput(script)
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			set[line] = true
		}
	}
	return set
}

// devMatchesSourceDisk reports whether a /proc/mounts device field is a
// partition of (or an LVM logical volume on) the disk named diskBase.
// The mapper check requires the disk name at an LV-name word boundary so
// an unrelated volume merely containing it as a substring (disk "sda",
// LV "vg-mysda2") does not match.
func devMatchesSourceDisk(dev, diskBase string) bool {
	if dev == "" || diskBase == "" {
		return false
	}
	if ok, _ := regexp.MatchString(`^/dev/`+regexp.QuoteMeta(diskBase)+`(p[0-9]+|[0-9]+)$`, dev); ok {
		return true
	}
	if strings.HasPrefix(dev, "/dev/mapper/") {
		segs := strings.Split(strings.TrimPrefix(dev, "/dev/mapper/"), "-")
		lv := segs[len(segs)-1]
		if lv == diskBase {
			return true
		}
		if strings.HasPrefix(lv, diskBase) && len(lv) > len(diskBase) {
			suffix := lv[len(diskBase)]
			return suffix == 'p' || (suffix >= '0' && suffix <= '9')
		}
	}
	return false
}

// preReadWarnIfMounted flushes pending writes on the source and warns loudly
// if any partition of the source disk is still mounted (i.e. the OS is live
// and writing concurrently with our dd read — produces a torn image).
//
// This is the #1 defense against the "ext4 bad block bitmap checksum" /
// "Journal has aborted" corruption seen after restoring a backup that was
// taken while the source system was live.
func (j *CloneJob) preReadWarnIfMounted() {
	// Flush all pending writes on the remote so whatever is in the page
	// cache hits the disk before we start reading. Cheap insurance.
	j.logFn("  Flushing remote filesystem buffers (sync)...")
	j.runner.CombinedOutput("sync")

	mounted, err := j.mountedSourcePartitions(j.params.SourcePath)
	if err != nil {
		j.logFn("  [!] 无法读取远程挂载表,跳过挂载状态警告: %v", err)
		return
	}
	if len(mounted) > 0 {
		j.logFn("  [!] 警告: 源磁盘的以下分区仍处于挂载状态:")
		for _, mp := range mounted {
			j.logFn("        - %s", mp)
		}
		j.logFn("  [!] 远程系统可能仍在运行,备份的镜像可能不一致!")
		j.logFn("  [!] 强烈建议先重启进入 Alpine RAM OS 再备份")
		j.logFn("  [!] 若克隆前仍无法卸载这些分区,任务将在 dd 开始前中止")
	}
}

// preReadSyncAndVerify does a final sync on the source and returns an error
// (aborting the transfer) if any of its partitions are still mounted. This
// MUST run after zero-fill and initramfs rebuild (which mount/unmount source
// partitions) and before dd.
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
func (j *CloneJob) preReadSyncAndVerify(src string) error {
	// Final sync — flush everything zero-fill / initramfs wrote.
	j.runner.CombinedOutput("sync; sync; sync")

	// Give the kernel a moment to finish flushing.
	time.Sleep(2 * time.Second)

	stillMounted, err := j.mountedSourcePartitions(src)
	if err != nil {
		return err
	}
	if len(stillMounted) > 0 {
		// Try one more regular umount (NOT lazy). If it fails we must abort.
		for _, mp := range stillMounted {
			j.runner.CombinedOutput("umount " + shellQuote(mp) + " 2>/dev/null")
		}
	}
	if err := j.abortIfSourceMounted(src); err != nil {
		return err
	}
	j.logFn("  ✓ 源盘所有分区已卸载,文件系统状态一致")
	return nil
}

// abortIfSourceMounted is the hard "source partitions must be unmounted"
// gate extracted from preReadSyncAndVerify so other pre-transfer steps can
// enforce the same abort. Re-reads the mount table and fails closed on a
// read error.
func (j *CloneJob) abortIfSourceMounted(src string) error {
	stillMounted, err := j.mountedSourcePartitions(src)
	if err != nil {
		return err
	}
	if len(stillMounted) == 0 {
		return nil
	}
	j.logFn("  [!] 严重: 源盘的以下分区无法卸载,dd 读到的镜像将不一致:")
	for _, mp := range stillMounted {
		j.logFn("        - %s", mp)
	}
	j.logFn("  [!] 请手动 umount 后重试,不要使用 lazy umount (-l)")
	return fmt.Errorf("source partitions still mounted: %s — aborted to avoid producing a corrupt image",
		strings.Join(stillMounted, ", "))
}

// ensureSourceUnmountedForZeroFill gates zero-fill on a fully unmounted
// source. zero-fill mounts each source partition and writes until the
// filesystem is full, so on a live system it would fill the production disk
// to 100% long before the pre-dd abort fires. This cannot be replaced by
// moving preReadSyncAndVerify earlier: that step must stay right before dd
// (after zero-fill/initramfs, before the freeze) so it flushes what those
// steps wrote — a frozen disk cannot be mounted for the fill.
func (j *CloneJob) ensureSourceUnmountedForZeroFill() error {
	mounted, err := j.mountedSourcePartitions(j.params.SourcePath)
	if err != nil {
		return err
	}
	if len(mounted) == 0 {
		return nil
	}
	j.logFn("  [!] 零填充要求源盘完全未挂载,以下分区仍处于挂载状态:")
	for _, mp := range mounted {
		j.logFn("        - %s", mp)
	}
	return fmt.Errorf("zero-fill requires the source disk fully unmounted (mounted: %s) — boot into the RAM OS or disable zero-fill",
		strings.Join(mounted, ", "))
}

// freezeSource freezes the remote block device to take a stable snapshot
// for dd to read. Best-effort: silently ignored if the kernel/busybox
// blockdev doesn't support it.
func (j *CloneJob) freezeSource() {
	j.runner.CombinedOutput(fmt.Sprintf("blockdev --freeze %s 2>/dev/null", j.params.SourcePath))
}

// unfreezeSource releases the freeze taken by freezeSource. Best-effort.
func (j *CloneJob) unfreezeSource() {
	j.runner.CombinedOutput(fmt.Sprintf("blockdev --unfreeze %s 2>/dev/null", j.params.SourcePath))
}

// preReadSafetyCheck and postReadUnfreeze are kept for backwards
// compatibility but no longer used by Run/RunToFile. The freeze/unfreeze
// is now split so that source-modifying steps (zero-fill, initramfs) run
// BEFORE the freeze, not while frozen.
//
// Deprecated: use preReadWarnIfMounted + preReadSyncAndVerify + freezeSource.
func (j *CloneJob) preReadSafetyCheck() {
	j.preReadWarnIfMounted()
	j.runner.CombinedOutput(fmt.Sprintf("blockdev --freeze %s 2>/dev/null", j.params.SourcePath))
}

func (j *CloneJob) postReadUnfreeze() {
	j.runner.CombinedOutput(fmt.Sprintf("blockdev --unfreeze %s 2>/dev/null", j.params.SourcePath))
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
		j.runner.CombinedOutput("command -v pigz || apk add --quiet pigz 2>/dev/null")
		if j.remoteHasCommand("pigz") {
			ncpus, _ := j.runner.CombinedOutput("nproc 2>/dev/null")
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
	j.runner.CombinedOutput("command -v gzip || apk add --quiet gzip 2>/dev/null")
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

// CompressToolName exposes compressToolName so callers can log which
// compressor actually ran (pigz silently falls back to gzip when the
// remote has no pigz).
func (j *CloneJob) CompressToolName() string { return j.compressToolName() }

// watchInterrupt handles Ctrl+C during a transfer. First press signals the
// remote process and marks cancelled; second press force-exits. os.Exit
// skips deferred cleanups, so onForceExit (source-disk unfreeze for
// save/clone paths) runs first with a hard deadline — a remote disk left
// in blockdev --freeze state hangs all later I/O until it reboots.
func (j *CloneJob) watchInterrupt(session sshclient.Session, done <-chan struct{}, cancelled *atomic.Bool, onForceExit func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	for {
		select {
		case <-sigCh:
			if !cancelled.Load() {
				cancelled.Store(true)
				_ = session.Signal(ssh.SIGTERM)
				j.logFn("  [!] 正在取消... (再按一次 Ctrl+C 强制退出)")
			} else {
				if onForceExit != nil {
					fin := make(chan struct{})
					go func() {
						defer close(fin)
						onForceExit()
					}()
					select {
					case <-fin:
					case <-time.After(5 * time.Second):
					}
				}
				os.Exit(130)
			}
		case <-done:
			return
		}
	}
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
		if err := j.ensureSourceUnmountedForZeroFill(); err != nil {
			return err
		}
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
	// after restore. Abort rather than produce a corrupt image.
	if err := j.preReadSyncAndVerify(j.params.SourcePath); err != nil {
		return err
	}

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

// partialFileSuffix marks the in-progress image during RunToFile; the file
// is renamed over the final name only after the transfer and the SHA256
// pass complete, so a failed run never destroys a pre-existing backup.
const partialFileSuffix = ".partial"

// RunToFile saves remote disk as a gzip compressed file.
// Remote does dd | gzip, only compressed data travels over the network.
// The file is a standard RFC 1952 gzip — compatible with gunzip/dd everywhere.
// With CompressionLevel == 0 the image is written uncompressed (raw .img).
func (j *CloneJob) RunToFile() error {
	if err := validateDevicePath(j.params.SourcePath); err != nil {
		return err
	}
	// Write "<target>.partial" and rename over the target only after the
	// full transfer (checksum included) succeeds: creating the final name
	// up front truncates an existing backup before the transfer even
	// starts, and a run that fails hours in must leave it intact. A failed
	// run keeps the leftover .partial. rename(2) does not overwrite on
	// Windows, hence the explicit remove of the target first.
	partPath := j.params.TargetPath + partialFileSuffix
	// Create the partial output before the long pre-transfer steps so an
	// unwritable path fails immediately instead of after a zero-fill.
	f, err := os.Create(partPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}

	if err := j.runToSink(f); err != nil {
		f.Close()
		return err
	}

	// Flush to disk before renaming: the caller generates the .sha256 file
	// immediately after and may show "save complete", so the image must be
	// durable by then (protects against power loss right after saving).
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close file: %w", err)
	}
	if err := os.Remove(j.params.TargetPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing file: %w", err)
	}
	if err := os.Rename(partPath, j.params.TargetPath); err != nil {
		return fmt.Errorf("rename file: %w", err)
	}
	return nil
}

// RunToStream saves the remote disk into w — the same pipeline as RunToFile
// (warn mounted, zero-fill, initramfs rebuild, sync+verify, freeze) but with
// the image going to any io.Writer, e.g. a remote-storage upload stream that
// never touches the local disk. The SHA256 of the written stream is
// available in ChecksumHex after success. The caller owns w: call Abort()
// when this returns an error, Close() to finalize on success.
func (j *CloneJob) RunToStream(w io.Writer) error {
	if err := validateDevicePath(j.params.SourcePath); err != nil {
		return err
	}
	return j.runToSink(w)
}

// runToSink contains the source preparation and dd|gzip pipeline shared by
// RunToFile and RunToStream.
func (j *CloneJob) runToSink(out io.Writer) error {
	// Pre-read source preparation (order matters! see Run docs):
	//   warn mounted -> zero-fill -> initramfs -> sync+verify -> freeze -> dd.
	j.preReadWarnIfMounted()

	if j.params.ZeroFill {
		if err := j.ensureSourceUnmountedForZeroFill(); err != nil {
			return err
		}
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
	// which GRUB refuses to read ("error: unknown filesystem"). Abort
	// rather than write a corrupt file.
	if err := j.preReadSyncAndVerify(j.params.SourcePath); err != nil {
		return err
	}

	j.freezeSource()
	defer j.unfreezeSource()

	// Hash the output while streaming so no second read pass over the
	// (possibly multi-GB) image is needed for the .sha256 file.
	hasher := sha256.New()
	out = io.MultiWriter(out, hasher)

	if j.params.CompressionLevel > 0 {
		// Build the compress command first so compressToolName reflects the
		// actual tool (pigz may fall back to gzip).
		_ = j.buildCompressCmd()
		j.logFn("  Remote compressing (dd|%s -> net -> target)", j.compressToolName())
		if err := j.streamCompressedRaw(out); err != nil {
			return err
		}
	} else {
		j.logFn("  No compression (dd -> net -> target)")
		if err := j.streamRaw(out); err != nil {
			return err
		}
	}

	j.ChecksumHex = hex.EncodeToString(hasher.Sum(nil))
	return nil
}

// RestoreFromFile reads a local .img.gz file and writes it to a remote disk.
// Flow: local gzip file -> gunzip -> SSH stdin -> remote dd of=TARGET
func (j *CloneJob) RestoreFromFile(filePath string) error {
	if j.runner == nil {
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
		// Uncompressed size: prefer the .size sidecar written at save time
		// (exact for any size) over the gzip ISIZE footer, which is only
		// exact up to 4 GiB (it wraps modulo 2^32 above that).
		if uncompSize := ReadSizeFile(filePath); uncompSize > 0 {
			j.params.SourceSize = uncompSize
		} else if uncompSize := readGzipISize(filePath); uncompSize > 0 {
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

	session, err := j.runner.ExecuteStdin(remoteCmd)
	if err != nil {
		return fmt.Errorf("start remote dd: %w", err)
	}
	defer session.Close()

	// Capture stderr
	stderrCh := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(session.Stderr())
		stderrCh <- string(data)
	}()

	done := make(chan struct{})
	defer close(done)
	var cancelled atomic.Bool
	// Restore writes a remote disk but never freezes a source; no cleanup
	// needed on force-exit.
	go j.watchInterrupt(session, done, &cancelled, nil)

	// Copy decompressed data to remote dd via SSH stdin
	written, copyErr := j.copyWithProgress(session.Stdin(), src, &cancelled, session)

	// Close stdin to signal EOF to remote dd
	session.Stdin().Close()

	// If the local side failed, tear the channel down too so a dd stuck on
	// a full target disk (or a dead connection) can't hang the Wait below.
	if copyErr != nil {
		session.Close()
	}

	sessionErr := session.Wait()

	// Force an additional global sync on the remote so all filesystem
	// metadata is flushed to the underlying block device before we return.
	// conv=fdatasync in dd only syncs the dd output stream; a separate sync
	// guarantees the kernel has written all dirty pages from the block device.
	if !cancelled.Load() && copyErr == nil {
		j.runner.CombinedOutput("sync")
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
	infoOut, _ := j.runner.CombinedOutput(fmt.Sprintf(
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

# LVM logical volumes backed by partitions of THIS disk only.
# /sys/block/dm-N/slaves/ lists the PV partitions behind each dm device;
# a slave named <diskbase>* means the LV lives on the source disk. Filling
# LVs on other disks would waste hours writing disks the user didn't select.
lvm_lvs=""
for d in /sys/block/dm-*; do
  [ -d "$d/slaves" ] || continue
  name=$(cat "$d/dm/name" 2>/dev/null)
  [ -n "$name" ] || continue
  owned=0
  for s in "$d"/slaves/*; do
    sname=${s##*/}
    case "$sname" in
      "$diskbase"|"$diskbase"p[0-9]*|"$diskbase"[0-9]*) owned=1 ;;
    esac
  done
  if [ "$owned" = "1" ] && [ -b "/dev/mapper/$name" ]; then
    lvm_lvs="$lvm_lvs /dev/mapper/$name"
  fi
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
    fill_rc=$?
    if [ $fill_rc -ne 0 ]; then echo "DDFAIL $dev rc=$fill_rc"; fi
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
	session, err := j.runner.Execute("sh -c " + shellQuote(script))
	if err != nil {
		return fmt.Errorf("start zero-fill: %w", err)
	}
	defer session.Close()

	scanner := bufio.NewScanner(session.Stdout())
	filled := 0
	skipped := 0
	var fillFailures []string
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
		} else if strings.HasPrefix(line, "UMOUNTFAIL ") {
			dev := strings.TrimPrefix(line, "UMOUNTFAIL ")
			fillFailures = append(fillFailures, "unmount "+dev)
			j.logFn("  [!] Warning: could not unmount %s after zero-fill", dev)
		} else if strings.HasPrefix(line, "DDFAIL ") {
			dev := strings.TrimPrefix(line, "DDFAIL ")
			fillFailures = append(fillFailures, "fill "+dev)
			j.logFn("  [!] Warning: zero-fill dd failed on %s", dev)
		} else if line == "NO_PARTS" {
			j.logFn("    No partitions found (raw disk)")
		} else if line == "DONE" {
			break
		}
	}

	if err := scanner.Err(); err != nil {
		j.logFn("  Warning: output stream ended early: %v", err)
	}

	// Surface remote failures instead of printing a false "Zero-fill done":
	// a dead connection or OOM-killed script used to be reported as success.
	if err := session.Wait(); err != nil {
		return fmt.Errorf("zero-fill script failed: %w", err)
	}
	if len(fillFailures) > 0 {
		return fmt.Errorf("zero-fill incomplete: %s", strings.Join(fillFailures, "; "))
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
	if j.runner == nil {
		return fmt.Errorf("SSH client is nil")
	}
	if targetDisk != "" {
		if err := validateDevicePath(targetDisk); err != nil {
			return fmt.Errorf("invalid target disk: %w", err)
		}
	}

	if targetDisk != "" {
		j.logFn("  重建引导 (GRUB + initramfs) 以兼容不同硬件...")
	} else {
		j.logFn("  Rebuilding initramfs (--no-hostonly for cross-hardware boot)...")
	}

	// Step 1: install LVM tools and activate volumes
	j.runner.CombinedOutput("apk add --quiet lvm2 2>/dev/null")
	j.runner.CombinedOutput("lvm vgscan --mknodes 2>/dev/null; lvm vgchange -ay 2>/dev/null")

	// Root-partition candidates must come from the disk being repaired —
	// the target disk in restore mode, the source disk in save mode.
	// Scanning every partition on the machine could pick another disk's
	// root and rebuild the wrong system.
	scanDisk := targetDisk
	if scanDisk == "" {
		scanDisk = j.params.SourcePath
	}
	out, _ := j.runner.CombinedOutput(fmt.Sprintf(
		`lsblk -ln -o NAME,TYPE,FSTYPE %s 2>/dev/null | awk '$2=="part"&&$3!=""&&$3!="swap"{print "/dev/"$1}'; lsblk -ln -o NAME,TYPE,FSTYPE %s 2>/dev/null | awk '$2=="lvm"&&$3!=""&&$3!="swap"{print "/dev/mapper/"$1}'`,
		shellQuote(scanDisk), shellQuote(scanDisk)))

	rootDev := ""
	for _, dev := range strings.Split(out, "\n") {
		dev = strings.TrimSpace(dev)
		if dev == "" {
			continue
		}

		// Check device exists before attempting mount (device names come
		// from remote lsblk output — always quote before embedding).
		check, _ := j.runner.CombinedOutput("test -b " + shellQuote(dev) + " && echo OK")
		if !strings.Contains(check, "OK") {
			continue
		}

		// Try mounting to see if it's root. rmdir must run on every path —
		// the old `&&`-chained form skipped it whenever mount failed and
		// could never reach the trailing rmdir after the brace group's exit.
		_, rcErr := j.runner.CombinedOutput(fmt.Sprintf(
			`rc=1
mp=$(mktemp -d)
if mount %s "$mp" 2>/dev/null; then
  if [ -f "$mp/etc/os-release" ] || [ -f "$mp/etc/fstab" ]; then rc=0; fi
  umount "$mp" 2>/dev/null
fi
rmdir "$mp" 2>/dev/null
exit $rc`,
			shellQuote(dev),
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

# Unmount everything we may have mounted under /mnt. Shared by the
# early-abort exits below and the normal exit — the early exits used to
# leak the mounts they had already made.
cleanup_mnt() {
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
}

# Unmount /mnt if something is already mounted there
umount /mnt 2>/dev/null

mount "$ROOT" /mnt 2>/dev/null || { echo "FAIL mount"; exit 1; }
[ ! -d /mnt/usr/bin ] && [ ! -d /mnt/usr/sbin ] && { echo "FAIL noroot"; cleanup_mnt; exit 1; }

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
    if mount "$BOOTDEV" /mnt/boot 2>/dev/null; then
      echo "  mounted /boot <- $BOOTDEV"
    else
      # A separate /boot that cannot be mounted means initramfs/GRUB work
      # would write into a shadowed directory on the root partition and
      # silently vanish when the real /boot comes back — abort instead.
      echo "BOOT_MOUNT_FAILED"
      cleanup_mnt
      exit 1
    fi
  fi
fi
EFIENT=$(awk '!/^[[:space:]]*#/ && $2=="/boot/efi" {print $1; exit}' /mnt/etc/fstab 2>/dev/null)
if [ -n "$EFIENT" ]; then
  EFIDEV=$(parse_fstab_dev "$EFIENT")
  if [ -n "$EFIDEV" ] && [ -b "$EFIDEV" ]; then
    mkdir -p /mnt/boot/efi 2>/dev/null
    if mount "$EFIDEV" /mnt/boot/efi 2>/dev/null; then
      echo "  mounted /boot/efi <- $EFIDEV"
    else
      echo "  warn: could not mount /boot/efi (UEFI GRUB install may fail)"
    fi
  fi
fi

# Where grub-install must place the EFI binary: /boot/efi when fstab has
# such an entry; systems whose vfat ESP is mounted at /boot directly (no
# /boot/efi entry) need /boot — hardcoding /boot/efi writes the EFI binary
# into a plain directory on the root fs and grub-install still reports
# success.
EFIDIR=/boot/efi
if [ -z "$EFIENT" ]; then
  BOOTTYPE=$(awk '!/^[[:space:]]*#/ && $2=="/boot" {print $3; exit}' /mnt/etc/fstab 2>/dev/null)
  if [ "$BOOTTYPE" = "vfat" ] && [ -d /mnt/boot/EFI ]; then
    EFIDIR=/boot
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

  # Boot mode follows the machine's own firmware: this tool runs on the box
  # that will boot the disk, and /sys/firmware/efi exists only on UEFI
  # boots. Guessing from directory layout alone misdetected UEFI systems
  # (ESP at /boot or /efi) as BIOS and wrote a broken legacy install.
  FWEFI=0
  [ -d /sys/firmware/efi ] && FWEFI=1

  GRUB_RC=1
  MKCFG_RC=0
  GRUBNAME=""
  CFGNAME=""
  CFGFILE=""
  if [ -x /mnt/usr/sbin/grub2-install ]; then
    GRUBNAME=grub2-install
    [ -x /mnt/usr/sbin/grub2-mkconfig ] && CFGNAME=grub2-mkconfig && CFGFILE=/boot/grub2/grub.cfg
  elif [ -x /mnt/usr/sbin/grub-install ]; then
    GRUBNAME=grub-install
    [ -x /mnt/usr/sbin/grub-mkconfig ] && CFGNAME=grub-mkconfig && CFGFILE=/boot/grub/grub.cfg
  else
    echo "NO_GRUB_TOOL"
  fi

  if [ -n "$GRUBNAME" ]; then
    if [ "$FWEFI" = "1" ]; then
      echo "  -> $GRUBNAME --target=x86_64-efi --efi-directory=$EFIDIR (UEFI firmware)"
      chroot /mnt /usr/sbin/$GRUBNAME --target=x86_64-efi --efi-directory=$EFIDIR --recheck 2>&1
      GRUB_RC=$?
    else
      echo "  -> $GRUBNAME --recheck $TARGETDISK (BIOS/Legacy firmware)"
      chroot /mnt /usr/sbin/$GRUBNAME --recheck "$TARGETDISK" 2>&1
      GRUB_RC=$?
    fi
    # grub.cfg regeneration is tracked separately: a stale grub.cfg (old
    # UUIDs) boots as badly as a failed install, so its failure must be
    # reported, not swallowed.
    if [ $GRUB_RC -eq 0 ] && [ -n "$CFGNAME" ]; then
      echo "  -> $CFGNAME -o $CFGFILE"
      chroot /mnt /usr/sbin/$CFGNAME -o "$CFGFILE" 2>&1
      MKCFG_RC=$?
    fi
  fi
  [ "$GRUBNAME" = "" ] && echo "GRUB_INSTALL_FAILED" && RC=1
  [ "$GRUBNAME" != "" ] && [ $GRUB_RC -ne 0 ] && echo "GRUB_INSTALL_FAILED" && RC=1
  [ $MKCFG_RC -ne 0 ] && echo "GRUB_MKCONFIG_FAILED" && RC=1

  # --- Fix fstab: comment out mounts for devices that no longer exist ---
  if [ -f /mnt/etc/fstab ]; then
    cp /mnt/etc/fstab /mnt/etc/fstab.bak 2>/dev/null
    awk '
      /^[[:space:]]*#/ { print; next }
      {
        dev=$1; mp=$2
        if (mp == "" || mp == "none" || mp == "swap") { print; next }
        if (mp == "/" || mp == "/boot" || mp ~ /^\/boot\//) { print; next }
        # Virtual/pseudo filesystems have no backing block device by design;
        # the existence check below would comment them out even though they
        # are valid on any machine.
        if ($3 ~ /^(tmpfs|proc|sysfs|devpts|devtmpfs|cgroup|cgroup2|ramfs|fusectl)$/) { print; next }
        if (dev == "none" || dev == "tmpfs") { print; next }
        # Resolve the device to a real /dev node. UUID=/LABEL= must be
        # resolved via command output (getline), NOT by pasting the blkid
        # command into [ -b ] -- that would always be false and comment out
        # every valid entry. Values coming from the restored fstab are
        # validated against a strict charset before being embedded in any
        # shell command (a crafted fstab must not gain code execution here).
        actual=""
        if (dev ~ /^UUID=/) {
          val=substr(dev,6)
          if (val ~ /^[A-Za-z0-9._:-]+$/) {
            cmd="blkid -U " val " 2>/dev/null"
            cmd | getline actual
            close(cmd)
          }
        } else if (dev ~ /^LABEL=/) {
          val=substr(dev,7)
          if (val ~ /^[A-Za-z0-9._:-]+$/) {
            cmd="blkid -L " val " 2>/dev/null"
            cmd | getline actual
            close(cmd)
          }
        } else if (dev ~ /^\/dev\/[A-Za-z0-9\/._-]+$/) actual=dev
        gsub(/[[:space:]]/, "", actual)
        if (actual != "" && actual ~ /^\/dev\/[A-Za-z0-9\/._-]+$/ && system("[ -b " actual " ] 2>/dev/null") == 0) {
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
# dd reads a "dirty" image. cleanup_mnt does regular umounts with retries,
# in reverse order (deepest first). Bind mounts first, then real mounts.
cleanup_mnt
exit $RC
`, shellQuote(rootDev), shellQuote(targetDisk))

	out2, err2 := j.runner.CombinedOutput("sh -c " + shellQuote(script))

	// Classify by output markers FIRST: the script exits non-zero (RC=1)
	// both for mount failures and for GRUB-install failure, so checking
	// err2 before the markers would hide the specific GRUB_INSTALL_FAILED
	// message behind the generic one. Failures are now returned so callers
	// (and the process exit code) can react instead of only log lines.
	switch {
	case strings.Contains(out2, "NO_INITRAMFS_TOOL"):
		j.logFn("  [!] No initramfs tool found (dracut/update-initramfs/mkinitcpio)")
		return fmt.Errorf("boot repair: no initramfs tool in the restored system (dracut/update-initramfs/mkinitcpio)")
	case strings.Contains(out2, "FAIL mount"):
		j.logFn("  [!] Failed to mount root partition")
		return fmt.Errorf("boot repair: failed to mount root partition %s", rootDev)
	case strings.Contains(out2, "FAIL noroot"):
		j.logFn("  [!] Mounted partition does not look like a root filesystem")
		return fmt.Errorf("boot repair: mounted partition does not look like a root filesystem")
	case strings.Contains(out2, "BOOT_MOUNT_FAILED"):
		j.logFn("  [!] Failed to mount the separate /boot partition — aborted")
		return fmt.Errorf("boot repair: failed to mount the separate /boot partition (rebuilding initramfs into a shadowed /boot would silently be lost)")
	// Must be matched before GRUB_INSTALL_FAILED: the script emits both
	// markers when no GRUB tool exists. grub-install being absent is normal
	// on systemd-boot/EFISTUB systems and initramfs was already rebuilt, so
	// this stays a warning (logged after the switch) instead of an error.
	case strings.Contains(out2, "NO_GRUB_TOOL"):
	case strings.Contains(out2, "GRUB_INSTALL_FAILED"):
		j.logFn("  [!] Initramfs rebuilt, but GRUB reinstall failed — you may need to run grub2-install manually")
		return fmt.Errorf("boot repair: GRUB reinstall failed (initramfs was rebuilt); run grub-install --recheck manually")
	case strings.Contains(out2, "GRUB_MKCONFIG_FAILED"):
		j.logFn("  [!] GRUB reinstalled but grub.cfg generation failed — run grub-mkconfig manually")
		return fmt.Errorf("boot repair: GRUB config regeneration failed (GRUB was reinstalled); run grub-mkconfig manually")
	case err2 != nil:
		j.logFn("  [!] Boot repair failed — you may need to fix boot manually")
		return fmt.Errorf("boot repair script failed: %v", err2)
	}
	if strings.Contains(out2, "NO_GRUB_TOOL") {
		j.logFn("  [!] No GRUB install tool found — skipped GRUB reinstall (normal for systemd-boot/EFISTUB); initramfs was rebuilt")
	} else if targetDisk != "" {
		j.logFn("  ✓ GRUB reinstalled and initramfs rebuilt")
	} else {
		j.logFn("  ✓ Initramfs rebuilt")
	}
	return nil
}

// streamCompressed: remote dd|gzip -> SSH -> local gzip.Reader -> dst
// This transfers compressed data over the network, then decompresses locally.
func (j *CloneJob) streamCompressed(dst io.Writer) error {
	if j.runner == nil {
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
	bsNum, _ := strconv.ParseInt(bsBytes, 10, 64)

	// Remote: dd | compress (gzip or pigz)
	compress := j.buildCompressCmd()
	// pipefail makes the pipeline's exit status reflect a dead dd; without
	// it gzip exits 0 on the EOF dd leaves behind and a truncated stream
	// passes as success. Shells without pipefail (dash) print an error —
	// silenced into /dev/null — and behave exactly as before.
	remoteCmd := fmt.Sprintf("set -o pipefail 2>/dev/null; dd if=%s bs=%s | %s", j.params.SourcePath, bsBytes, compress)

	session, err := j.runner.Execute(remoteCmd)
	if err != nil {
		return fmt.Errorf("start remote dd|gzip: %w", err)
	}
	defer session.Close()

	// Capture stderr
	stderrCh := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(session.Stderr())
		stderrCh <- string(data)
	}()

	// Handle Ctrl+C: first press sends SIGTERM to remote, second force-exits
	// (unfreezing the source first — see watchInterrupt).
	done := make(chan struct{})
	defer close(done)
	var cancelled atomic.Bool
	go j.watchInterrupt(session, done, &cancelled, j.unfreezeSource)

	// Decompress on-the-fly and write to target
	gzr, err := gzip.NewReader(session.Stdout())
	if err != nil {
		return fmt.Errorf("init gzip decompressor: %w (remote gzip may not be installed)", err)
	}
	defer gzr.Close()

	written, copyErr := j.copyWithProgress(dst, gzr, &cancelled, session)
	if copyErr != nil {
		// The local side stopped reading; the remote dd|gzip may be stuck
		// on a full SSH window. Close the channel so Wait can't block
		// forever.
		session.Close()
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
		errMsg := "remote dd produced no data -- disk may be busy, missing, or inaccessible"
		if stderrOut != "" {
			errMsg += "\n  stderr: " + strings.TrimSpace(stderrOut)
		}
		finalErr = fmt.Errorf("%s", errMsg)
	} else {
		// Verify dd actually read the full source disk (GNU dd reports bytes
		// read on stderr; BusyBox dd does not, so the check is skipped there).
		if ddRead := parseDdBytesRead(stderrOut); ddRead > 0 && j.params.SourceSize > 0 && ddRead < j.params.SourceSize {
			finalErr = fmt.Errorf("clone truncated: dd read %d bytes but source disk is %d bytes (%.1f%% of expected) — target disk content is incomplete",
				ddRead, j.params.SourceSize, float64(ddRead)/float64(j.params.SourceSize)*100)
		} else if recs := parseDdRecordsOut(stderrOut); ddRecordsTruncated(recs, bsNum, j.params.SourceSize) {
			// Locale-independent fallback for the missing "N bytes" line:
			// the "X+Y records out" count bounds the bytes dd moved from
			// below.
			finalErr = fmt.Errorf("clone truncated: dd wrote at most %d bytes (%d full blocks) but source disk is %d bytes — target disk content is incomplete",
				recs*bsNum+bsNum-1, recs, j.params.SourceSize)
		}
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
	if j.runner == nil {
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
	bsNum, _ := strconv.ParseInt(bsBytes, 10, 64)

	compress := j.buildCompressCmd()
	remoteCmd := fmt.Sprintf("set -o pipefail 2>/dev/null; dd if=%s bs=%s | %s", j.params.SourcePath, bsBytes, compress)

	session, err := j.runner.Execute(remoteCmd)
	if err != nil {
		return fmt.Errorf("start remote dd|gzip: %w", err)
	}
	defer session.Close()

	stderrCh := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(session.Stderr())
		stderrCh <- string(data)
	}()

	done := make(chan struct{})
	defer close(done)
	var cancelled atomic.Bool
	go j.watchInterrupt(session, done, &cancelled, j.unfreezeSource)

	// Tee the compressed stream into dst while decompressing a copy to count
	// real disk bytes for the progress bar (see function doc above).
	fw := &errWriter{w: dst}
	gzr, err := gzip.NewReader(io.TeeReader(session.Stdout(), fw))
	if err != nil {
		return fmt.Errorf("init gzip decompressor: %w (remote gzip may not be installed)", err)
	}
	defer gzr.Close()

	written, copyErr := j.copyWithProgress(io.Discard, gzr, &cancelled, session)
	if copyErr == nil && fw.err != nil {
		copyErr = fmt.Errorf("write error: %w", fw.err)
	}
	if copyErr != nil {
		// The local side stopped reading; tear the channel down so the
		// remote pipeline (stuck on a full SSH window) dies and Wait
		// can't block forever.
		session.Close()
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
		} else if recs := parseDdRecordsOut(stderrOut); ddRecordsTruncated(recs, bsNum, j.params.SourceSize) {
			// Locale-independent fallback for the missing "N bytes" line:
			// the "X+Y records out" count bounds the bytes dd moved from
			// below.
			finalErr = fmt.Errorf("backup truncated: dd wrote at most %d bytes (%d full blocks) but source disk is %d bytes — backup is corrupt, do not restore it",
				recs*bsNum+bsNum-1, recs, j.params.SourceSize)
		}
	}

	j.progressFn(Progress{Done: true, Error: finalErr})
	return finalErr
}

// streamRaw: remote dd -> SSH -> dst (no compression in pipe, caller wraps in gzip if needed)
func (j *CloneJob) streamRaw(dst io.Writer) error {
	if j.runner == nil {
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
	bsNum, _ := strconv.ParseInt(bsBytes, 10, 64)

	remoteCmd := fmt.Sprintf("dd if=%s bs=%s", j.params.SourcePath, bsBytes)

	session, err := j.runner.Execute(remoteCmd)
	if err != nil {
		return fmt.Errorf("start remote dd: %w", err)
	}
	defer session.Close()

	stderrCh := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(session.Stderr())
		stderrCh <- string(data)
	}()

	done := make(chan struct{})
	defer close(done)
	var cancelled atomic.Bool
	go j.watchInterrupt(session, done, &cancelled, j.unfreezeSource)

	written, copyErr := j.copyWithProgress(dst, session.Stdout(), &cancelled, session)
	if copyErr != nil {
		// The local side stopped writing; the remote dd may be stuck on a
		// full SSH window. Close the channel so Wait can't block forever.
		session.Close()
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
	} else {
		// Same truncation check as streamCompressedRaw: a dd that exits
		// early must not yield a silently-short raw .img with a valid hash.
		if ddRead := parseDdBytesRead(stderrOut); ddRead > 0 && j.params.SourceSize > 0 && ddRead < j.params.SourceSize {
			finalErr = fmt.Errorf("backup truncated: dd read %d bytes but source disk is %d bytes (%.1f%% of expected) — backup is corrupt, do not restore it",
				ddRead, j.params.SourceSize, float64(ddRead)/float64(j.params.SourceSize)*100)
		} else if recs := parseDdRecordsOut(stderrOut); ddRecordsTruncated(recs, bsNum, j.params.SourceSize) {
			finalErr = fmt.Errorf("backup truncated: dd wrote at most %d bytes (%d full blocks) but source disk is %d bytes — backup is corrupt, do not restore it",
				recs*bsNum+bsNum-1, recs, j.params.SourceSize)
		}
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

// copyWithProgress copies src to dst while reporting progress once per
// second. cancelled lets the Ctrl+C handler stop the copy locally instead of
// relying on the remote process dying from SIGTERM (which dropbear, for
// example, does not support). stallCloser (the SSH session) is closed by a
// watchdog if the connection dies while no data is flowing — otherwise the
// blocked Read would hang forever.
func (j *CloneJob) copyWithProgress(dst io.Writer, src io.Reader, cancelled *atomic.Bool, stallCloser io.Closer) (int64, error) {
	buf := make([]byte, 4*1024*1024) // 4MB
	var written int64
	start := time.Now()
	lastUpdate := time.Now()
	var lastWritten int64

	// Stall watchdog: if the SSH connection dies while the copy is blocked
	// in Read (no data, no EOF), closing the session unblocks it. If the
	// connection is alive but silent, warn once and keep waiting.
	var lastData atomic.Int64
	lastData.Store(time.Now().Unix())
	watchDone := make(chan struct{})
	defer close(watchDone)
	if stallCloser != nil {
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			warnedStall := false
			for {
				select {
				case <-watchDone:
					return
				case <-ticker.C:
					idle := time.Since(time.Unix(lastData.Load(), 0))
					if idle < 60*time.Second {
						continue
					}
					if !j.runner.IsConnected() {
						j.logFn("  [!] SSH 连接已断开 (已 %d 秒没有数据), 强制中断传输", int(idle.Seconds()))
						_ = stallCloser.Close()
						return
					}
					if !warnedStall {
						j.logFn("  [!] 已 %d 秒没有收到数据 (连接仍存活, 继续等待)...", int(idle.Seconds()))
						warnedStall = true
					}
				}
			}
		}()
	}

	// Sliding window of speed samples (1 per second, last 30 seconds)
	var speedRing [30]float64
	var speedRingPos int
	var speedRingCount int
	warnedSlow := false

	for {
		if cancelled != nil && cancelled.Load() {
			return written, fmt.Errorf("cancelled by user")
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			lastData.Store(time.Now().Unix())
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
				if !j.runner.IsConnected() {
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
