package storage

import (
	"strings"
	"testing"
)

func TestParseURLSFTP(t *testing.T) {
	cfg, err := ParseURL("sftp://backup:s3cret@nas.lan:2022/volume1/backups/disk.img.gz")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	if cfg.Kind != KindSFTP {
		t.Errorf("Kind = %q, want sftp", cfg.Kind)
	}
	if cfg.Host != "nas.lan" || cfg.Port != 2022 || cfg.User != "backup" || cfg.Password != "s3cret" {
		t.Errorf("conn = %+v", cfg)
	}
	if cfg.Path != "/volume1/backups/disk.img.gz" {
		t.Errorf("Path = %q", cfg.Path)
	}
	if BaseName(cfg) != "disk.img.gz" {
		t.Errorf("BaseName = %q", BaseName(cfg))
	}
}

func TestParseURLFTPDefaults(t *testing.T) {
	cfg, err := ParseURL("ftp://anon@server.lan/dir/img.img.gz")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	if cfg.Kind != KindFTP || cfg.Port != 21 {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.Path != "dir/img.img.gz" {
		t.Errorf("Path = %q (ftp paths must be relative to the login dir)", cfg.Path)
	}
}

func TestParseURLWebDAV(t *testing.T) {
	cfg, err := ParseURL("davs://user:pw@nas.lan:5006/dav/dir/file.img.gz")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	if cfg.Kind != KindWebDAV {
		t.Errorf("Kind = %q", cfg.Kind)
	}
	if cfg.URL != "https://nas.lan:5006/dav/dir/file.img.gz" {
		t.Errorf("URL = %q", cfg.URL)
	}
	// The display form must never leak the password.
	if d := Describe(cfg); strings.Contains(d, "pw@") {
		t.Errorf("Describe leaks credentials: %q", d)
	}
}

func TestParseURLS3(t *testing.T) {
	cfg, err := ParseURL("s3://AKID:SECRET@s3.amazonaws.com/mybucket/dir/obj.img.gz?region=ap-east-1&path=1")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	if cfg.Kind != KindS3 || cfg.Endpoint != "s3.amazonaws.com" || cfg.Bucket != "mybucket" ||
		cfg.Key != "dir/obj.img.gz" || cfg.Region != "ap-east-1" || !cfg.PathStyle || !cfg.UseTLS {
		t.Errorf("cfg = %+v", cfg)
	}
	// Defaults: us-east-1, https, virtual-host style.
	cfg2, err := ParseURL("s3://ak:sk@minio.lan:9000/bkt/obj.img")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	if cfg2.Region != "us-east-1" || !cfg2.UseTLS || cfg2.PathStyle || cfg2.Endpoint != "minio.lan:9000" {
		t.Errorf("defaults: %+v", cfg2)
	}
}

func TestParseURLRejects(t *testing.T) {
	for _, bad := range []string{
		"http://example.com/x", // unsupported scheme
		"sftp://host",          // no path
		"s3://host",            // no bucket/key
		"s3://host/bucket",     // no key
	} {
		if _, err := ParseURL(bad); err == nil {
			t.Errorf("ParseURL(%q) = nil error, want error", bad)
		}
	}
}

func TestAppendName(t *testing.T) {
	cfg := Config{Kind: KindSFTP, Path: "/backup/"}
	cfg.AppendName("a.img.gz")
	if cfg.Path != "/backup/a.img.gz" || cfg.NeedsName() {
		t.Errorf("sftp AppendName: %q", cfg.Path)
	}
	cfg2 := Config{Kind: KindS3, Key: "dir/"}
	cfg2.AppendName("b.img.gz")
	if cfg2.Key != "dir/b.img.gz" {
		t.Errorf("s3 AppendName: %q", cfg2.Key)
	}
	cfg3 := Config{Kind: KindWebDAV, URL: "http://x/dav/"}
	cfg3.AppendName("c.img.gz")
	if cfg3.URL != "http://x/dav/c.img.gz" {
		t.Errorf("webdav AppendName: %q", cfg3.URL)
	}
}

func TestAWSURIEncode(t *testing.T) {
	if got := awsURIEncode("a b/c+d", false); got != "a%20b/c%2Bd" {
		t.Errorf("awsURIEncode(path) = %q", got)
	}
	if got := awsURIEncode("a b/c+d", true); got != "a%20b%2Fc%2Bd" {
		t.Errorf("awsURIEncode(query) = %q", got)
	}
	if got := awsURIEncode("~-_.", true); got != "~-_." {
		t.Errorf("awsURIEncode(unreserved) = %q", got)
	}
	if got := awsURIEncode("ü", true); got != "%C3%BC" {
		t.Errorf("awsURIEncode(utf8) = %q", got)
	}
}
