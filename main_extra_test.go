package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestValidIPv4(t *testing.T) {
	for _, ok := range []string{"192.168.1.1", "0.0.0.0", "255.255.255.255", "10.0.0.1"} {
		if !validIPv4(ok) {
			t.Errorf("validIPv4(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"999.999.999.999", "256.1.1.1", "1.2.3", "1.2.3.4.5", "..", ""} {
		if validIPv4(bad) {
			t.Errorf("validIPv4(%q) = true, want false", bad)
		}
	}
}

// TestExtractIPGarbageNotExtracted: an out-of-range dotted number embedded
// in pasted text must not be extracted as if it were a valid address.
func TestExtractIPGarbageNotExtracted(t *testing.T) {
	if got := extractIP("IP: 999.1.1.1"); got != "IP: 999.1.1.1" {
		t.Errorf("extractIP(garbage) = %q, want input returned unchanged", got)
	}
	if got := extractIP("IP: 192.168.1.100"); got != "192.168.1.100" {
		t.Errorf("extractIP(valid) = %q, want 192.168.1.100", got)
	}
	if got := extractIP("nas.lan"); got != "nas.lan" {
		t.Errorf("extractIP(hostname) = %q, want hostname passthrough", got)
	}
}

// TestVerifyChecksumFailsClosed: a present-but-broken .sha256 must count as
// verification FAILURE, not silent success.
func TestVerifyChecksumFailsClosed(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "disk.img.gz")
	if err := os.WriteFile(img, []byte("image-bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	// Valid checksum → true.
	sum := sha256.Sum256([]byte("image-bytes"))
	if err := os.WriteFile(img+".sha256", []byte(fmt.Sprintf("%x  disk.img.gz\n", sum)), 0644); err != nil {
		t.Fatal(err)
	}
	if !verifyChecksum(img) {
		t.Fatal("verifyChecksum(valid) = false, want true")
	}

	// Empty checksum file → false.
	if err := os.WriteFile(img+".sha256", []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	if verifyChecksum(img) {
		t.Fatal("verifyChecksum(empty .sha256) = true, want false")
	}

	// Garbage checksum file → false.
	if err := os.WriteFile(img+".sha256", []byte("not-a-hash\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if verifyChecksum(img) {
		t.Fatal("verifyChecksum(garbage .sha256) = true, want false")
	}

	// Wrong checksum → false.
	if err := os.WriteFile(img+".sha256", []byte(fmt.Sprintf("%064x  disk.img.gz\n", 1)), 0644); err != nil {
		t.Fatal(err)
	}
	if verifyChecksum(img) {
		t.Fatal("verifyChecksum(mismatch) = true, want false")
	}
}
