package clone

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	sshclient "disk-cloner/internal/ssh"
	"golang.org/x/crypto/ssh"
)

type Params struct {
	SourcePath string
	TargetPath string // block device for clone, file path for save
	SourceSize int64
	BlockSize  string
	ZeroFill   bool // zero-fill free space before dd (improves compression)
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

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// remoteHasCommand checks if a command exists on the remote server.
func (j *CloneJob) remoteHasCommand(cmd string) bool {
	_, err := j.sshClient.CombinedOutput("command -v " + cmd)
	return err == nil
}

// Run clones remote disk to a local block device.
// Uses gzip compression over SSH to reduce network transfer:
//   remote: dd | gzip → SSH → local: gunzip → write to disk
// Also does zero-fill beforehand to maximize compression.
func (j *CloneJob) Run() error {
	if err := validateDevicePath(j.params.SourcePath); err != nil {
		return err
	}
	if err := validateDevicePath(j.params.TargetPath); err != nil {
		return err
	}

	// Zero-fill free space on remote disk to improve compression ratio
	if err := j.zeroFillFreeSpace(); err != nil {
		j.logFn("  ⚠ 零填充失败 (继续克隆): %v", err)
	}

	target, err := os.OpenFile(j.params.TargetPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open target device: %w", err)
	}
	defer target.Close()

	// Check if remote has gzip (Alpine busybox includes it, but check anyway)
	hasGzip := j.remoteHasCommand("gzip")

	if hasGzip {
		j.logFn("  使用压缩传输 (dd|gzip → 网络 → gunzip → 磁盘)")
		if err := j.streamCompressed(target); err != nil {
			return err
		}
	} else {
		j.logFn("  ⚠ 远程未安装 gzip，使用原始传输 (速度较慢)")
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
// The remote does dd | gzip, so only compressed data travels over the network.
func (j *CloneJob) RunToFile() error {
	if err := validateDevicePath(j.params.SourcePath); err != nil {
		return err
	}

	// Zero-fill free space before dd to improve gzip compression
	if j.params.ZeroFill {
		if err := j.zeroFillFreeSpace(); err != nil {
			j.logFn("  ⚠ 零填充失败 (继续保存): %v", err)
		}
	}

	f, err := os.Create(j.params.TargetPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	// Remote compresses, local writes the compressed stream directly to disk.
	// No local gzip — the file is already gzip-compressed by the remote.
	j.logFn("  远程压缩传输 (dd|gzip → 网络 → 本地文件)")
	if err := j.streamCompressedRaw(f); err != nil {
		return err
	}

	return nil
}

// zeroFillFreeSpace mounts each partition on the source disk,
// writes zeros to fill free space, then unmounts.
func (j *CloneJob) zeroFillFreeSpace() error {
	j.logFn("  正在对远程磁盘空闲空间写零 (提高压缩率)...")

	diskName := j.params.SourcePath

	script := fmt.Sprintf(`disk="%s"
diskbase=$(basename "$disk")
modprobe ext4 2>/dev/null
modprobe xfs 2>/dev/null
modprobe btrfs 2>/dev/null
modprobe vfat 2>/dev/null
parts=""
for p in /sys/block/"$diskbase"/"$diskbase"*/partition; do
  [ -f "$p" ] || continue
  pname=$(basename $(dirname "$p"))
  parts="$parts /dev/$pname"
done
if [ -z "$parts" ]; then
  echo "NO_PARTS"
  exit 0
fi
for part in $parts; do
  mp=$(mktemp -d /tmp/zf.XXXXXX)
  if mount "$part" "$mp" 2>/dev/null; then
    echo "FILL $part"
    dd if=/dev/zero of="$mp/.zero_fill" bs=4194304 2>/dev/null || true
    rm -f "$mp/.zero_fill"
    sync
    umount "$mp" 2>/dev/null || umount -l "$mp" 2>/dev/null
  else
    echo "SKIP $part"
  fi
  rmdir "$mp" 2>/dev/null || true
done
echo "DONE"
`, diskName)

	out, err := j.sshClient.CombinedOutput("sh -c " + shellQuote(script))
	if err != nil {
		if strings.Contains(out, "DONE") || strings.Contains(out, "FILL") {
			// Partial success is OK
		} else {
			return fmt.Errorf("zero-fill: %w\n%s", err, out)
		}
	}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "FILL ") {
			j.logFn("    ✓ 已填充: %s", strings.TrimPrefix(line, "FILL "))
		} else if strings.HasPrefix(line, "SKIP ") {
			j.logFn("    - 跳过: %s", strings.TrimPrefix(line, "SKIP "))
		} else if line == "NO_PARTS" {
			j.logFn("    未发现分区")
		}
	}
	j.logFn("  ✓ 零填充完成")
	return nil
}

// streamCompressed: remote dd|gzip → SSH → local gzip.Reader → dst
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

	// Remote: dd | gzip -1 (fast compression)
	// gzip -1 is best for dd streaming: minimal CPU overhead, good compression on zero-filled regions
	remoteCmd := fmt.Sprintf("dd if=%s bs=%s | gzip -1", j.params.SourcePath, bsBytes)

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

	// Handle Ctrl+C
	done := make(chan struct{})
	defer close(done)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	cancelled := false
	go func() {
		select {
		case <-sigCh:
			cancelled = true
			_ = session.Signal(ssh.SIGTERM)
		case <-done:
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
	if cancelled {
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
		errMsg := "remote dd produced no data — disk may be busy, missing, or inaccessible"
		if stderrOut != "" {
			errMsg += "\n  stderr: " + strings.TrimSpace(stderrOut)
		}
		finalErr = fmt.Errorf("%s", errMsg)
	}

	j.progressFn(Progress{Done: true, Error: finalErr})
	return finalErr
}

// streamCompressedRaw: remote dd|gzip → SSH → dst (no local decompression).
// The remote sends already-compressed data; we write it directly to disk.
// This is used by RunToFile where the output file is already .gz format.
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

	remoteCmd := fmt.Sprintf("dd if=%s bs=%s | gzip -1", j.params.SourcePath, bsBytes)

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
	cancelled := false
	go func() {
		select {
		case <-sigCh:
			cancelled = true
			_ = session.Signal(ssh.SIGTERM)
		case <-done:
		}
	}()

	// Write compressed bytes directly — no local gzip/decompression
	written, copyErr := j.copyWithProgress(dst, session.Stdout)

	sessionErr := session.Wait()

	stderrOut := ""
	select {
	case stderrOut = <-stderrCh:
	case <-time.After(3 * time.Second):
	}

	var finalErr error
	if cancelled {
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
		errMsg := "remote dd|gzip produced no data — disk may be busy, missing, or gzip not installed"
		if stderrOut != "" {
			errMsg += "\n  stderr: " + strings.TrimSpace(stderrOut)
		}
		finalErr = fmt.Errorf("%s", errMsg)
	}

	j.progressFn(Progress{Done: true, Error: finalErr})
	return finalErr
}

// streamRaw: remote dd → SSH → dst (no compression in pipe, caller wraps in gzip if needed)
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
	cancelled := false
	go func() {
		select {
		case <-sigCh:
			cancelled = true
			_ = session.Signal(ssh.SIGTERM)
		case <-done:
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
	if cancelled {
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
		errMsg := "remote dd produced no data — disk may be busy, missing, or inaccessible"
		if stderrOut != "" {
			errMsg += "\n  stderr: " + strings.TrimSpace(stderrOut)
		}
		finalErr = fmt.Errorf("%s", errMsg)
	}

	j.progressFn(Progress{Done: true, Error: finalErr})
	return finalErr
}

func (j *CloneJob) copyWithProgress(dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 4*1024*1024) // 4MB
	var written int64
	start := time.Now()
	lastUpdate := time.Now()
	var lastWritten int64

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

				speedMBps := 0.0
				if interval > 0 {
					speedMBps = float64(written-lastWritten) / interval / (1024 * 1024)
				}

				percent := 0.0
				eta := int64(0)
				if j.params.SourceSize > 0 {
					percent = float64(written) / float64(j.params.SourceSize) * 100
					if percent > 100 {
						percent = 100
					}
					if speedMBps > 0 {
						remaining := j.params.SourceSize - written
						if remaining > 0 {
							eta = int64(float64(remaining) / (speedMBps * 1024 * 1024))
						}
					}
				}

				j.progressFn(Progress{
					BytesWritten:   written,
					TotalBytes:     j.params.SourceSize,
					Percent:        percent,
					SpeedMBps:      speedMBps,
					ElapsedSeconds: int64(elapsed),
					EtaSeconds:     eta,
				})

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
