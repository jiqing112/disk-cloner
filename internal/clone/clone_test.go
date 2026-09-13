package clone

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReadSizeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.img.gz")
	WriteSizeFile(path, 53_687_091_200) // 50 GiB
	if got := ReadSizeFile(path); got != 53_687_091_200 {
		t.Fatalf("ReadSizeFile = %d, want 53687091200", got)
	}
	if got := ReadSizeFile(filepath.Join(t.TempDir(), "missing.img.gz")); got != 0 {
		t.Fatalf("missing sidecar: ReadSizeFile = %d, want 0", got)
	}
	if err := os.WriteFile(path+sizeFileSuffix, []byte("not-a-number"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := ReadSizeFile(path); got != 0 {
		t.Fatalf("garbage sidecar: ReadSizeFile = %d, want 0", got)
	}
}

func TestValidateBlockSize(t *testing.T) {
	for _, ok := range []string{"4M", "1M", "512K", "8G", "1048576", "2k"} {
		if err := ValidateBlockSize(ok); err != nil {
			t.Errorf("ValidateBlockSize(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"4 MB", "4MiB", "; rm -rf /", "M4", ""} {
		if err := ValidateBlockSize(bad); err == nil {
			t.Errorf("ValidateBlockSize(%q) = nil, want error", bad)
		}
	}
}

func TestBsToBytes(t *testing.T) {
	cases := map[string]string{
		"4M": "4194304", "1G": "1073741824", "512k": "524288",
		"1048576": "1048576", "": "4194304",
	}
	for in, want := range cases {
		if got := bsToBytes(in); got != want {
			t.Errorf("bsToBytes(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGzipUncompressedSize(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	payload := bytes.Repeat([]byte("A"), 1024)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "small.img.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	if got := GzipUncompressedSize(path); got != int64(len(payload)) {
		t.Fatalf("GzipUncompressedSize = %d, want %d", got, len(payload))
	}
}
