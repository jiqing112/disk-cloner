// Package storage streams a disk image to remote storage — SFTP, FTP,
// WebDAV or S3-compatible object storage — without touching the local disk.
// All backends expose the same streaming Writer so the remote dd|gzip
// pipeline can feed them directly.
package storage

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Supported storage backends.
const (
	KindSFTP       = "sftp"
	KindFTP        = "ftp"
	KindWebDAV     = "webdav"
	KindS3         = "s3"
	KindPixelDrain = "pixeldrain"
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
	// are meant for trusted networks. Set via the tlsverify=1 URL param or
	// the -tls-verify CLI flag.
	InsecureTLS bool

	// Logf, when set, receives backend warnings (e.g. the WebDAV spool
	// fallback) during Open. Optional.
	Logf func(format string, args ...interface{})
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
	case KindPixelDrain:
		return openPixelDrain(cfg)
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
	case KindPixelDrain:
		scheme := "https"
		if !cfg.UseTLS {
			scheme = "http"
		}
		return fmt.Sprintf("pixeldrain://%s @ %s:%d (%s)", cfg.Path, cfg.Host, cfg.Port, scheme)
	}
	return cfg.Kind
}

// BaseName returns the file name part of the destination — used for naming
// the local .sha256/.size/.log sidecar files.
func BaseName(cfg Config) string {
	switch cfg.Kind {
	case KindSFTP, KindFTP, KindPixelDrain:
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
	case KindSFTP, KindFTP, KindPixelDrain:
		if c.Path == "" || strings.HasSuffix(c.Path, "/") {
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
// (or is empty for s3/pixeldrain) and still needs a file name appended.
func (c *Config) NeedsName() bool {
	switch c.Kind {
	case KindSFTP, KindFTP:
		return strings.HasSuffix(c.Path, "/")
	case KindPixelDrain:
		return c.Path == "" || strings.HasSuffix(c.Path, "/")
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
//	pixeldrain://:APIKEY@pixeldrain.com/file.img.gz   (别名 pd://)
//
// Percent-encode special characters in user/password (e.g. %40 for '@').
// For s3, query params: region (default us-east-1), path=1 (path-style),
// tls=0 (plain HTTP). webdav:// / webdavs:// are accepted as aliases.
// For pixeldrain the password IS the API key (the username is ignored, an
// empty username keeps the URL readable); tls=0 targets a plain-HTTP mirror
// (handy for local testing).
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
	// tlsverify=1 opts into certificate verification for https endpoints
	// (davs/s3). Default remains insecure — see the InsecureTLS docs.
	q := u.Query()
	if q.Get("tlsverify") == "1" {
		cfg.InsecureTLS = false
	}
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
		// s3://access:secret@endpoint/... — userinfo carries the object
		// storage credentials.
		cfg.AccessKey = cfg.User
		cfg.SecretKey = cfg.Password
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
	case "pixeldrain", "pd":
		cfg.Kind = KindPixelDrain
		cfg.Host = u.Hostname()
		if cfg.Host == "" {
			cfg.Host = "pixeldrain.com"
		}
		cfg.Port = portOr(u.Port(), 443)
		cfg.Path = strings.TrimPrefix(u.Path, "/")
		cfg.UseTLS = q.Get("tls") != "0"
		// The API key goes in the password position; also accept
		// pd://KEY@host/... for readability.
		if cfg.Password == "" && cfg.User != "" {
			cfg.Password = cfg.User
			cfg.User = ""
		}
		if cfg.Password == "" {
			return cfg, fmt.Errorf("pixeldrain URL needs an API key (pixeldrain://:APIKEY@pixeldrain.com/file.img.gz)")
		}
	default:
		return cfg, fmt.Errorf("unsupported storage scheme %q (use sftp:// ftp:// dav:// davs:// s3:// pixeldrain://)", u.Scheme)
	}
	// Percent-decoded credentials/paths can carry CR/LF/NUL that would allow
	// control-channel or header injection on the wire — reject them here.
	for _, s := range []string{cfg.User, cfg.Password, cfg.Path, cfg.URL,
		cfg.Endpoint, cfg.Bucket, cfg.Key, cfg.AccessKey, cfg.SecretKey} {
		if hasCtl(s) {
			return cfg, fmt.Errorf("storage URL components must not contain control characters")
		}
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

// retry runs fn up to n times with linear backoff, retrying only transient
// failures (transport errors, HTTP 5xx / 408 signalled via *s3PartError).
// Non-retryable errors return immediately. Used for buffered, replayable
// requests — never for the live image stream itself.
func retry(n int, fn func() error) error {
	var err error
	for attempt := 0; attempt < n; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		err = fn()
		if err == nil {
			return nil
		}
		var oe *httpOpError
		if !errors.As(err, &oe) || !oe.retryable {
			return err
		}
	}
	return err
}

// hasCtl reports whether s contains characters that would break line- or
// header-oriented protocols (FTP control commands, HTTP headers): C0 controls,
// DEL. Percent-decoding can smuggle these past url.Parse, so every decoded
// config component is checked before use.
func hasCtl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}
