// Package storage streams a disk image to remote storage — SFTP, FTP,
// WebDAV or S3-compatible object storage — without touching the local disk.
// All backends expose the same streaming Writer so the remote dd|gzip
// pipeline can feed them directly.
package storage

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// Supported storage backends.
const (
	KindSFTP   = "sftp"
	KindFTP    = "ftp"
	KindWebDAV = "webdav"
	KindS3     = "s3"
)

// Config describes one remote storage destination.
type Config struct {
	Kind string

	// Connection used by sftp/ftp.
	Host     string
	Port     int
	User     string
	Password string

	// sftp/ftp: destination file path (ftp may be relative to the login dir).
	Path string

	// webdav: complete file URL (http:// or https://).
	URL string

	// s3
	Endpoint  string // host[:port], e.g. s3.amazonaws.com or minio.lan:9000
	UseTLS    bool
	Region    string
	Bucket    string
	Key       string // object key, no bucket prefix
	AccessKey string
	SecretKey string
	PathStyle bool // path-style addressing (MinIO and most self-hosted S3)

	// InsecureTLS skips certificate verification for https (webdav/s3).
	// The tool's SSH layer likewise does not pin host keys; these transfers
	// are meant for trusted networks.
	InsecureTLS bool
}

// Writer streams bytes to the destination. Close finalizes the upload
// (S3 CompleteMultipartUpload, FTP data-connection close, ...); it also
// cleans up any partial remote data when it returns an error. Abort
// discards a failed upload's partial data (best effort) without attempting
// a finalize.
type Writer interface {
	io.WriteCloser
	Abort() error
}

// Open validates the destination and returns a streaming writer. It fails
// fast — before any disk data is transferred — so bad credentials, paths or
// permissions surface before a hours-long zero-fill, not after it.
func Open(cfg Config) (Writer, error) {
	switch cfg.Kind {
	case KindSFTP:
		return openSFTP(cfg)
	case KindFTP:
		return openFTP(cfg)
	case KindWebDAV:
		return openWebDAV(cfg)
	case KindS3:
		return openS3(cfg)
	default:
		return nil, fmt.Errorf("storage: unknown kind %q", cfg.Kind)
	}
}

// Describe renders the destination for display and logging. It never
// includes the password or secret key.
func Describe(cfg Config) string {
	switch cfg.Kind {
	case KindSFTP:
		return fmt.Sprintf("sftp://%s@%s:%d%s", cfg.User, cfg.Host, cfg.Port, cfg.Path)
	case KindFTP:
		return fmt.Sprintf("ftp://%s@%s:%d%s", cfg.User, cfg.Host, cfg.Port, cfg.Path)
	case KindWebDAV:
		return cfg.URL
	case KindS3:
		scheme := "https"
		if !cfg.UseTLS {
			scheme = "http"
		}
		style := "virtual-host"
		if cfg.PathStyle {
			style = "path-style"
		}
		return fmt.Sprintf("s3://%s/%s @ %s (%s, %s)", cfg.Bucket, cfg.Key, cfg.Endpoint, scheme, style)
	}
	return cfg.Kind
}

// BaseName returns the file name part of the destination — used for naming
// the local .sha256/.size/.log sidecar files.
func BaseName(cfg Config) string {
	switch cfg.Kind {
	case KindSFTP, KindFTP:
		return path.Base(cfg.Path)
	case KindWebDAV:
		if u, err := url.Parse(cfg.URL); err == nil {
			return path.Base(u.Path)
		}
	case KindS3:
		return path.Base(cfg.Key)
	}
	return "disk-image"
}

// AppendName appends the file name to destinations that end with a directory
// separator (CLI auto naming).
func (c *Config) AppendName(name string) {
	switch c.Kind {
	case KindSFTP, KindFTP:
		if strings.HasSuffix(c.Path, "/") {
			c.Path += name
		}
	case KindWebDAV:
		if strings.HasSuffix(c.URL, "/") {
			c.URL += name
		}
	case KindS3:
		if c.Key == "" || strings.HasSuffix(c.Key, "/") {
			c.Key += name
		}
	}
}

// NeedsName reports whether the destination ends with a directory separator
// (or is empty for s3) and still needs a file name appended.
func (c *Config) NeedsName() bool {
	switch c.Kind {
	case KindSFTP, KindFTP:
		return strings.HasSuffix(c.Path, "/")
	case KindWebDAV:
		return strings.HasSuffix(c.URL, "/")
	case KindS3:
		return c.Key == "" || strings.HasSuffix(c.Key, "/")
	}
	return false
}

// ParseURL parses a CLI destination URL:
//
//	sftp://user:pass@host:22/dir/file.img.gz
//	ftp://user:pass@host:21/dir/file.img.gz
//	dav://user:pass@host:port/dir/file.img.gz    (WebDAV over HTTP)
//	davs://user:pass@host:port/dir/file.img.gz   (WebDAV over HTTPS)
//	s3://access:secret@endpoint[:port]/bucket/key?region=xx&path=1&tls=0
//
// Percent-encode special characters in user/password (e.g. %40 for '@').
// For s3, query params: region (default us-east-1), path=1 (path-style),
// tls=0 (plain HTTP). webdav:// / webdavs:// are accepted as aliases.
func ParseURL(raw string) (Config, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Config{}, fmt.Errorf("invalid storage URL: %w", err)
	}
	cfg := Config{InsecureTLS: true}
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Password, _ = u.User.Password()
	}
	switch strings.ToLower(u.Scheme) {
	case "sftp":
		cfg.Kind = KindSFTP
		cfg.Host = u.Hostname()
		cfg.Port = portOr(u.Port(), 22)
		cfg.Path = u.Path
		if cfg.Path == "" || cfg.Path == "/" {
			return cfg, fmt.Errorf("sftp URL needs a file path")
		}
	case "ftp":
		cfg.Kind = KindFTP
		cfg.Host = u.Hostname()
		cfg.Port = portOr(u.Port(), 21)
		cfg.Path = strings.TrimPrefix(u.Path, "/") // ftp paths are relative to the login dir
		if cfg.Path == "" {
			return cfg, fmt.Errorf("ftp URL needs a file path")
		}
	case "dav", "webdav", "davs", "webdavs":
		cfg.Kind = KindWebDAV
		scheme := "http"
		if strings.HasSuffix(strings.ToLower(u.Scheme), "s") {
			scheme = "https"
		}
		cfg.URL = scheme + "://" + u.Host + u.Path
		if u.Path == "" || u.Path == "/" {
			return cfg, fmt.Errorf("webdav URL needs a file path")
		}
	case "s3":
		cfg.Kind = KindS3
		cfg.Endpoint = u.Host
		if cfg.Endpoint == "" {
			return cfg, fmt.Errorf("s3 URL needs an endpoint host")
		}
		q := u.Query()
		cfg.Region = q.Get("region")
		cfg.PathStyle = q.Get("path") == "1"
		cfg.UseTLS = q.Get("tls") != "0"
		p := strings.TrimPrefix(u.Path, "/")
		bucket, key, found := strings.Cut(p, "/")
		if bucket == "" {
			return cfg, fmt.Errorf("s3 URL needs bucket/key")
		}
		cfg.Bucket = bucket
		if found {
			cfg.Key = key
		}
		// A trailing slash means "append the auto name later" (NeedsName);
		// anything else without a key is simply incomplete.
		if cfg.Key == "" && !strings.HasSuffix(p, "/") {
			return cfg, fmt.Errorf("s3 URL needs an object key (or a trailing slash for auto naming)")
		}
		if cfg.Region == "" {
			cfg.Region = "us-east-1"
		}
	default:
		return cfg, fmt.Errorf("unsupported storage scheme %q (use sftp:// ftp:// dav:// davs:// s3://)", u.Scheme)
	}
	return cfg, nil
}

func portOr(p string, def int) int {
	if p == "" {
		return def
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// httpClient returns a client suitable for long streaming uploads: no
// overall timeout (transfers run for hours), TLS verification configurable.
func httpClient(insecure bool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
		},
	}
}
