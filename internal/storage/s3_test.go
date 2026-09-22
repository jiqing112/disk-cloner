package storage

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestV4SignatureAWSVector checks the signer against the worked example in
// the AWS S3 "Authenticating Requests (Signature Version 4)" documentation.
func TestV4SignatureAWSVector(t *testing.T) {
	headers := map[string]string{
		"host":                 "examplebucket.s3.amazonaws.com",
		"range":                "bytes=0-9",
		"x-amz-content-sha256": emptyPayloadHash,
		"x-amz-date":           "20130524T000000Z",
	}
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	sig := v4Signature(
		"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"us-east-1", "s3",
		"GET", "/test.txt", "",
		headers, emptyPayloadHash, now,
	)
	const want = "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if sig != want {
		t.Fatalf("signature = %s, want %s", sig, want)
	}
}

// fakeS3 implements just enough of the S3 API for the multipart flow:
// CreateMultipartUpload, UploadPart, CompleteMultipartUpload and DELETE
// (abort). Path-style addressing over plain HTTP.
type fakeS3 struct {
	mu          sync.Mutex
	uploadID    string
	parts       map[int][]byte
	etags       map[int]string
	completed   bool
	aborted     bool
	completeXML string
	failParts   bool
	authSeen    bool
	lastPath    string
}

func (s *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPath = r.URL.Path
	s.authSeen = strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=") &&
		r.Header.Get("x-amz-content-sha256") != ""
	if !s.authSeen {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.RawQuery == "uploads=":
		s.uploadID = "test-upload-id-123"
		fmt.Fprint(w, `<?xml version="1.0"?><InitiateMultipartUploadResult><UploadId>test-upload-id-123</UploadId></InitiateMultipartUploadResult>`)
	case r.Method == http.MethodPut && r.URL.Query().Get("partNumber") != "":
		if s.failParts {
			w.WriteHeader(http.StatusInsufficientStorage)
			fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>SlowDown</Code><Message>no space</Message></Error>`)
			return
		}
		n := 0
		fmt.Sscanf(r.URL.Query().Get("partNumber"), "%d", &n)
		body, _ := io.ReadAll(r.Body)
		if s.parts == nil {
			s.parts = map[int][]byte{}
			s.etags = map[int]string{}
		}
		s.parts[n] = body
		s.etags[n] = fmt.Sprintf(`"etag-%d"`, n)
		w.Header().Set("ETag", s.etags[n])
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && r.URL.Query().Get("uploadId") != "":
		body, _ := io.ReadAll(r.Body)
		s.completeXML = string(body)
		s.completed = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `<?xml version="1.0"?><CompleteMultipartUploadResult><ETag>"done"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete:
		s.aborted = true
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestS3MultipartEndToEnd(t *testing.T) {
	srv := &fakeS3{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := Config{
		Kind: KindS3, Endpoint: ts.Listener.Addr().String(),
		UseTLS: false, Region: "us-east-1", Bucket: "bkt", Key: "dir/x.img.gz",
		AccessKey: "AKID", SecretKey: "SECRET", PathStyle: true, InsecureTLS: true,
	}
	w, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// 20 MiB → 8+8+4 MiB parts with the default 8 MiB part size.
	payload := bytes.Repeat([]byte{0xA5}, 20<<20)
	if n, err := w.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.completed || srv.aborted {
		t.Fatalf("completed=%v aborted=%v", srv.completed, srv.aborted)
	}
	if len(srv.parts) != 3 {
		t.Fatalf("got %d parts, want 3", len(srv.parts))
	}
	var joined []byte
	for i := 1; i <= 3; i++ {
		joined = append(joined, srv.parts[i]...)
	}
	if !bytes.Equal(joined, payload) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", len(joined), len(payload))
	}
	for _, part := range []string{"<PartNumber>1</PartNumber>", "<PartNumber>2</PartNumber>",
		"<PartNumber>3</PartNumber>", `"etag-1"`, `"etag-3"`} {
		if !strings.Contains(srv.completeXML, part) {
			t.Errorf("complete XML missing %q: %s", part, srv.completeXML)
		}
	}
}

func TestS3AbortOnWriteFailure(t *testing.T) {
	srv := &fakeS3{failParts: true}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := Config{
		Kind: KindS3, Endpoint: ts.Listener.Addr().String(),
		UseTLS: false, Region: "us-east-1", Bucket: "bkt", Key: "x.img.gz",
		AccessKey: "AKID", SecretKey: "SECRET", PathStyle: true, InsecureTLS: true,
	}
	w, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s3w := w.(*s3Writer)
	if _, err := s3w.Write(make([]byte, s3w.partSize)); err == nil {
		t.Fatal("Write on failing server = nil error, want error")
	}
	// Close must abort the multipart upload, not complete it.
	if err := w.Close(); err == nil {
		t.Fatal("Close on failing server = nil error, want error")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.aborted {
		t.Fatal("multipart upload was not aborted after part failure")
	}
	if srv.completed {
		t.Fatal("multipart upload was completed despite part failure")
	}
}

// TestS3CreateMultipartFailureFailsFast ensures Open surfaces credential /
// permission problems before any data is streamed.
func TestS3CreateMultipartFailureFailsFast(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>bad creds</Message></Error>`)
	}))
	defer ts.Close()

	cfg := Config{
		Kind: KindS3, Endpoint: ts.Listener.Addr().String(),
		UseTLS: false, Region: "us-east-1", Bucket: "bkt", Key: "x",
		AccessKey: "AKID", SecretKey: "SECRET", PathStyle: true, InsecureTLS: true,
	}
	if _, err := Open(cfg); err == nil || !strings.Contains(err.Error(), "bad creds") {
		t.Fatalf("Open = %v, want AccessDenied message", err)
	}
}

// TestS3KeyEncodedOnceOnTheWire: the object key must be percent-encoded
// exactly once. It used to be encoded twice (pre-encoded into w.key, then
// the whole path again into RawPath), so keys with spaces or non-ASCII
// characters were stored under mojibake names like "%E5%A4%87%E4%BB%BD".
func TestS3KeyEncodedOnceOnTheWire(t *testing.T) {
	srv := &fakeS3{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	const key = "备份 dir/my file.img.gz"
	cfg := Config{
		Kind: KindS3, Endpoint: ts.Listener.Addr().String(),
		UseTLS: false, Region: "us-east-1", Bucket: "bkt", Key: key,
		AccessKey: "AKID", SecretKey: "SECRET", PathStyle: true, InsecureTLS: true,
	}
	w, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := w.Write([]byte("payload")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	// The server's decoded path must be exactly the original key — any
	// double encoding leaves literal %XX sequences behind after one decode.
	if want := "/bkt/" + key; srv.lastPath != want {
		t.Fatalf("server saw path %q, want %q", srv.lastPath, want)
	}
}

// TestS3CompleteRetriesTransportError: one dropped connection during
// CompleteMultipartUpload must be retried, not abort the whole multipart
// upload and destroy hours of transfer.
func TestS3CompleteRetriesTransportError(t *testing.T) {
	var completes int32
	srv := &fakeS3{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Query().Get("uploadId") != "" {
			if atomic.AddInt32(&completes, 1) == 1 {
				// Drop the connection without a reply — a transient network
				// hiccup at finalize time.
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, _ := hj.Hijack()
					conn.Close()
					return
				}
			}
		}
		srv.ServeHTTP(w, r)
	}))
	defer ts.Close()

	cfg := Config{
		Kind: KindS3, Endpoint: ts.Listener.Addr().String(),
		UseTLS: false, Region: "us-east-1", Bucket: "bkt", Key: "x.img.gz",
		AccessKey: "AKID", SecretKey: "SECRET", PathStyle: true, InsecureTLS: true,
	}
	w, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := w.Write([]byte("payload")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close after transport hiccup = %v, want retry to succeed", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if atomic.LoadInt32(&completes) < 2 {
		t.Fatalf("complete attempts = %d, want ≥ 2 (retry)", completes)
	}
	if !srv.completed || srv.aborted {
		t.Fatalf("completed=%v aborted=%v — retry must complete, never abort", srv.completed, srv.aborted)
	}
}
