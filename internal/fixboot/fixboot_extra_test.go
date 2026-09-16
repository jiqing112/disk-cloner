package fixboot

import (
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
