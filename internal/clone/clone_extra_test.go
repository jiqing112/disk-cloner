package clone

import (
	"bytes"
	"compress/gzip"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sshclient "disk-cloner/internal/ssh"
)

// TestValidateBlockSizeZeroAndOverflow covers inputs that match the old
// regex but were dangerous: zero sizes (dd rejects bs=0) and values that
// overflow int64 when converted to plain bytes for BusyBox dd.
func TestValidateBlockSizeZeroAndOverflow(t *testing.T) {
	for _, bad := range []string{"0", "0K", "0M", "9223372036854775807G", "9223372036854775807K"} {
		if err := ValidateBlockSize(bad); err == nil {
			t.Errorf("ValidateBlockSize(%q) = nil, want error", bad)
		}
	}
	// Large-but-representable values stay valid.
	for _, ok := range []string{"17179869184K", "8G", "4096"} {
		if err := ValidateBlockSize(ok); err != nil {
			t.Errorf("ValidateBlockSize(%q) = %v, want nil", ok, err)
		}
	}
}

func TestBsToBytesConversion(t *testing.T) {
	cases := map[string]string{
		"":     "4194304",
		"4M":   "4194304",
		"512K": "524288",
		"1G":   "1073741824",
		"4096": "4096",
		"2k":   "2048",
	}
	for in, want := range cases {
		if got := bsToBytes(in); got != want {
			t.Errorf("bsToBytes(%q) = %q, want %q", in, got, want)
		}
	}
	// Invalid input (callers validate first) falls back to the default
	// instead of producing a negative/overflowed byte count.
	if got := bsToBytes("9223372036854775807G"); got != "4194304" {
		t.Errorf("bsToBytes(overflow) = %q, want default", got)
	}
}

func writeGzip(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "img.gz")
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReadGzipISizeExactSmall: small files (< the wrap-possible compressed
// size) must report the exact ISIZE footer.
func TestReadGzipISizeExactSmall(t *testing.T) {
	data := bytes.Repeat([]byte{0xA5}, 4096)
	path := writeGzip(t, data)
	if got := readGzipISize(path); got != int64(len(data)) {
		t.Fatalf("readGzipISize = %d, want %d", got, len(data))
	}
}

// TestReadGzipISizeUnknownWhenWrapPossible: an incompressible image large
// enough that its true size could be ≥ 4 GiB (compressed > 4 GiB/1032) has
// an ambiguous footer — the function must report 0 (unknown) instead of a
// possibly wrapped-by-4-GiB value that earlier code passed off as exact.
func TestReadGzipISizeUnknownWhenWrapPossible(t *testing.T) {
	data := make([]byte, 5<<20) // 5 MiB of random data ≈ 5 MiB compressed
	rng := rand.New(rand.NewSource(42))
	rng.Read(data)
	path := writeGzip(t, data)
	if got := readGzipISize(path); got != 0 {
		t.Fatalf("readGzipISize = %d, want 0 (wrap cannot be ruled out)", got)
	}
}

// TestDevMatchesSourceDisk locks down the /proc/mounts device matching:
// partitions of the source disk and LVs on it match, while unrelated LVs
// that merely contain the disk name as a substring do not.
func TestDevMatchesSourceDisk(t *testing.T) {
	cases := []struct {
		dev, disk string
		want      bool
	}{
		{"/dev/sda1", "sda", true},
		{"/dev/sda", "sda", false},
		{"/dev/nvme0n1p1", "nvme0n1", true},
		{"/dev/vda2", "vda", true},
		{"/dev/sdb1", "sda", false},
		// sdaa1 must not match disk "sda" (the digit suffix anchors it).
		{"/dev/sdaa1", "sda", false},
		// LV named after the disk partition matches…
		{"/dev/mapper/vg-sda3", "sda", true},
		{"/dev/mapper/sda", "sda", true},
		{"/dev/mapper/vg-sdap1", "sda", true},
		// …but an unrelated LV embedding the name does not.
		{"/dev/mapper/vg-mysda2", "sda", false},
		{"/dev/mapper/vg-root", "sda", false},
		{"tmpfs", "sda", false},
	}
	for _, c := range cases {
		if got := devMatchesSourceDisk(c.dev, c.disk); got != c.want {
			t.Errorf("devMatchesSourceDisk(%q, %q) = %v, want %v", c.dev, c.disk, got, c.want)
		}
	}
}

// TestParseDdBytesRead covers the GNU dd stderr format used by the
// truncation checks.
func TestParseDdBytesRead(t *testing.T) {
	out := "1073741824 bytes (1.1 GB, 1.0 GiB) copied, 12.3 s, 87.3 MB/s"
	if got := parseDdBytesRead(out); got != 1073741824 {
		t.Fatalf("parseDdBytesRead = %d, want 1073741824", got)
	}
	if got := parseDdBytesRead("32768+0 records in\n32768+0 records out"); got != -1 {
		t.Fatalf("busybox-style output: parseDdBytesRead = %d, want -1", got)
	}
}

// TestParseDdRecordsOut covers the locale-independent fallback: the
// "X+Y records out" line is used when the GNU "N bytes copied" line is
// missing (BusyBox dd, non-English locale output without a bytes summary).
func TestParseDdRecordsOut(t *testing.T) {
	cases := []struct {
		name, stderr string
		want         int64
	}{
		// GNU coreutils: records line present alongside the bytes summary.
		{"GNU full output",
			"32768+0 records in\n32768+0 records out\n1073741824 bytes (1.1 GB) copied, 12 s, 87 MB/s", 32768},
		// BusyBox dd: no bytes line at all.
		{"busybox records only", "262144+1 records in\n262144+1 records out", 262144},
		// No parsable bytes line (localized summary), English records line.
		{"no bytes line", "1024+0 records in\n1024+0 records out", 1024},
		{"partial blocks only", "0+512 records out", 0},
		{"garbage", "disk read error\nnothing to see here", -1},
		{"records in is not records out", "1024+0 records in", -1},
	}
	for _, c := range cases {
		if got := parseDdRecordsOut(c.stderr); got != c.want {
			t.Errorf("%s: parseDdRecordsOut = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestDdRecordsTruncated locks down the lower-bound test: a complete read of
// sourceSize at block size bs ends with X*bs+bs > sourceSize, so the
// converse proves truncation, and unusable inputs prove nothing.
func TestDdRecordsTruncated(t *testing.T) {
	const mib = int64(1 << 20)
	cases := []struct {
		name               string
		recs, bs, sourceSz int64
		want               bool
	}{
		{"complete with partial tail block", 24, 4 * mib, 99 * mib, false},
		{"complete exact multiple", 25, 4 * mib, 100 * mib, false},
		{"one block short", 24, 4 * mib, 100 * mib, true},
		{"missing partial tail", 7, mib, 8 * mib, true},
		{"no full blocks at all", 0, 4 * mib, 100 * mib, true},
		{"records count unusable", -1, 4 * mib, 100 * mib, false},
		{"block size unknown", 24, 0, 100 * mib, false},
		{"source size unknown", 24, 4 * mib, 0, false},
		{"count beyond disk (overflow guard)", 1 << 62, 4 * mib, 100 * mib, false},
	}
	for _, c := range cases {
		if got := ddRecordsTruncated(c.recs, c.bs, c.sourceSz); got != c.want {
			t.Errorf("%s: ddRecordsTruncated(%d, %d, %d) = %v, want %v",
				c.name, c.recs, c.bs, c.sourceSz, got, c.want)
		}
	}
}

// fakeRunner answers CombinedOutput by substring-matching the command
// against canned outputs — enough to test the /proc/mounts + dm-slaves
// parsing logic without a real shell.
type fakeRunner struct {
	outputs map[string]string
}

func (f *fakeRunner) CombinedOutput(cmd string) (string, error) {
	for k, v := range f.outputs {
		if strings.Contains(cmd, k) {
			return v, nil
		}
	}
	return "", nil
}
func (f *fakeRunner) Execute(string) (sshclient.Session, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeRunner) ExecuteStdin(string) (sshclient.Session, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeRunner) IsConnected() bool { return true }

// TestMountedSourcePartitionsDetectsLVM: an LVM root mounted as
// /dev/mapper/vg0-root must be attributed to the source disk via the
// kernel's dm-slaves topology — name matching alone never matched real LV
// names (vg-root, vg-swap), so the pre-dd "abort if still mounted" gate
// let a live LVM system through and dd produced a torn image.
func TestMountedSourcePartitionsDetectsLVM(t *testing.T) {
	r := &fakeRunner{outputs: map[string]string{
		"/proc/mounts": "/dev/mapper/vg0-root / ext4 rw 0 0\n" +
			"/dev/sda1 /boot ext4 rw 0 0\n" +
			"/dev/sdb1 /data ext4 rw 0 0\n",
		"/sys/block/dm-": "/dev/mapper/vg0-root\n/dev/dm-0\n",
	}}
	j := &CloneJob{runner: r, params: Params{SourcePath: "/dev/sda"}}
	mounted, err := j.mountedSourcePartitions("/dev/sda")
	if err != nil {
		t.Fatalf("mountedSourcePartitions: %v", err)
	}

	want := map[string]bool{"/": false, "/boot": false}
	for _, mp := range mounted {
		if _, ok := want[mp]; ok {
			want[mp] = true
		}
	}
	for mp, seen := range want {
		if !seen {
			t.Errorf("mount %q of a source-disk device not detected (mounted=%v)", mp, mounted)
		}
	}
	for _, mp := range mounted {
		if mp == "/data" {
			t.Errorf("unrelated disk's mount %q wrongly attributed to the source disk", mp)
		}
	}
}
