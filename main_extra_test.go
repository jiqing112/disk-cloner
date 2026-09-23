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

// TestExtractHostPort: an out-of-range dotted number embedded in pasted text
// must not be extracted as if it were a valid address; a hostname containing
// an IP-shaped substring must pass through unchanged (extraction boundaries
// must not cut inside a hostname); a pasted "IP:port" yields both parts.
func TestExtractHostPort(t *testing.T) {
	cases := []struct {
		in        string
		wantHost  string
		wantPort  int
	}{
		{"IP: 999.1.1.1", "IP: 999.1.1.1", 0},      // garbage stays untouched
		{"IP: 192.168.1.100", "192.168.1.100", 0},   // plain extraction
		{"nas.lan", "nas.lan", 0},                   // hostname passthrough
		{"srv-192.168.1.5.lan", "srv-192.168.1.5.lan", 0}, // IP inside hostname must not be ripped out
		{"192.168.1.100:22", "192.168.1.100", 22},   // pasted port preserved
		{"IP: 10.0.0.5:2121", "10.0.0.5", 2121},     // port in pasted text
		{"root@192.168.1.7", "192.168.1.7", 0},      // user@host delimiter
		{"192.168.1.100:0", "192.168.1.100", 0},     // port 0 rejected
		{"192.168.1.100:99999", "192.168.1.100", 0}, // out-of-range port rejected
		{"1.2.3.4.5", "1.2.3.4.5", 0},               // trailing junk → not delimited, passthrough
	}
	for _, c := range cases {
		host, port := extractHostPort(c.in)
		if host != c.wantHost || port != c.wantPort {
			t.Errorf("extractHostPort(%q) = (%q, %d), want (%q, %d)", c.in, host, port, c.wantHost, c.wantPort)
		}
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
