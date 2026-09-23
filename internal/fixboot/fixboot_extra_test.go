package fixboot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDetectDistroUbuntuNotDebian: Ubuntu and Mint carry ID_LIKE=debian in
// os-release; the distro keyword scan must match the specific distro before
// the generic "debian" keyword, otherwise the GRUB bootloader-id is wrong.
func TestDetectDistroUbuntuNotDebian(t *testing.T) {
	cases := []struct {
		osRelease string
		want      string
	}{
		{"ID=ubuntu\nID_LIKE=debian\n", "ubuntu"},
		{"ID=linuxmint\nID_LIKE=debian\n", "linuxmint"},
		{"ID=debian\n", "debian"},
		{"ID=fedora\n", "fedora"},
		{"ID=alpine\n", "unknown"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "etc"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "etc", "os-release"), []byte(c.osRelease), 0644); err != nil {
			t.Fatal(err)
		}
		if got := detectDistro(dir); got != c.want {
			t.Errorf("detectDistro(%q) = %q, want %q", c.osRelease, got, c.want)
		}
	}
}

// TestFixFstabShortLine: a malformed 2-field entry must gain a real fstype
// column ("auto"), not "defaults" in the fstype position (which makes mount
// fail with "unknown filesystem type defaults").
func TestFixFstabShortLine(t *testing.T) {
	dir := t.TempDir()
	etc := filepath.Join(dir, "etc")
	if err := os.MkdirAll(etc, 0755); err != nil {
		t.Fatal(err)
	}
	// /opt/data is neither a protected mount nor in the comment-out list,
	// so it exercises the "add nofail" expansion path.
	const orig = "/dev/sda1 / ext4 defaults 0 1\n/dev/sdb1 /opt/data\n"
	if err := os.WriteFile(filepath.Join(etc, "fstab"), []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}

	if err := fixFstab(dir); err != nil {
		t.Fatalf("fixFstab: %v", err)
	}
	out, err := os.ReadFile(filepath.Join(etc, "fstab"))
	if err != nil {
		t.Fatal(err)
	}
	var dataLine string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "/dev/sdb1") {
			dataLine = line
		}
	}
	if dataLine == "" {
		t.Fatalf("data line missing from output:\n%s", out)
	}
	fields := strings.Fields(dataLine)
	if len(fields) != 6 {
		t.Fatalf("2-field line expanded to %d fields (%q), want 6", len(fields), dataLine)
	}
	if fields[2] != "auto" {
		t.Fatalf("fstype column = %q, want \"auto\"", fields[2])
	}
	if !strings.Contains(fields[3], "nofail") {
		t.Fatalf("options column = %q, want nofail", fields[3])
	}
	// Root entry must be untouched.
	if !strings.Contains(string(out), "/dev/sda1 / ext4 defaults 0 1") {
		t.Fatalf("root entry was modified:\n%s", out)
	}
}

// TestSlaveMatchesDisk: the dm-slaves filter accepts the disk itself and its
// partitions using the naming style of the disk (digit-ending names like
// nvme0n1 separate partition numbers with "p"), and must not be fooled by a
// longer disk name sharing the prefix (nvme0n10 vs nvme0n1).
func TestSlaveMatchesDisk(t *testing.T) {
	cases := []struct {
		slave, disk string
		want        bool
	}{
		{"sda", "sda", true},
		{"sda1", "sda", true},
		{"sda12", "sda", true},
		{"sdab", "sda", false},
		{"sdab1", "sda", false},
		{"sdb1", "sda", false},
		{"nvme0n1", "nvme0n1", true},
		{"nvme0n1p1", "nvme0n1", true},
		{"nvme0n1p12", "nvme0n1", true},
		{"nvme0n1p", "nvme0n1", false},
		{"nvme0n10", "nvme0n1", false},
		{"nvme0n10p1", "nvme0n1", false},
		{"nvme1n1p1", "nvme0n1", false},
		{"sda3", "nvme0n1", false},
		{"mmcblk0", "mmcblk0", true},
		{"mmcblk0p2", "mmcblk0", true},
		{"loop0p1", "loop0", true},
	}
	for _, c := range cases {
		if got := slaveMatchesDisk(c.slave, c.disk); got != c.want {
			t.Errorf("slaveMatchesDisk(%q, %q) = %v, want %v", c.slave, c.disk, got, c.want)
		}
	}
}

// TestDecideEFI: firmware is authoritative — a completed probe decides
// alone, so a BIOS machine restoring a UEFI-origin image (fstab/ESP hints
// present) stays BIOS; only an abnormal probe error falls back to hints.
func TestDecideEFI(t *testing.T) {
	notExist := fmt.Errorf("stat /sys/firmware/efi: %w", os.ErrNotExist)
	abnormal := errors.New("permission denied")

	cases := []struct {
		name     string
		probeErr error
		fwIsDir  bool
		fstabEFI bool
		espDev   string
		espDir   bool
		want     bool
	}{
		{"firmware says UEFI", nil, true, false, "", false, true},
		{"probe says file not dir", nil, false, true, "", false, false},
		{"firmware says BIOS, image hints UEFI", notExist, false, true, "/dev/sda1", true, false},
		{"abnormal probe, fstab hint", abnormal, false, true, "", false, true},
		{"abnormal probe, esp dev", abnormal, false, false, "/dev/sda1", false, true},
		{"abnormal probe, esp dir", abnormal, false, false, "", true, true},
		{"abnormal probe, no hint", abnormal, false, false, "", false, false},
	}
	for _, c := range cases {
		if got := decideEFI(c.probeErr, c.fwIsDir, c.fstabEFI, c.espDev, c.espDir); got != c.want {
			t.Errorf("%s: decideEFI = %v, want %v", c.name, got, c.want)
		}
	}
}
