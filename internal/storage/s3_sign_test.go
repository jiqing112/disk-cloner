package storage

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestCanonicalQueryPreservesKeyCase guards the SigV4 canonical-query
// builder: query keys must keep their original case. Lowercasing them made
// the client sign "partnumber/uploadid" while the server recomputes over
// "partNumber/uploadId" — every UploadPart returned 403
// SignatureDoesNotMatch.
func TestCanonicalQueryPreservesKeyCase(t *testing.T) {
	q := s3Query([2]string{"partNumber", "2"}, [2]string{"uploadId", "AbC_123"})
	if !strings.Contains(q, "partNumber=2") || !strings.Contains(q, "uploadId=AbC_123") {
		t.Fatalf("s3Query mangled the keys: %q", q)
	}
	// canonicalQueryOf must be idempotent over an already-canonical query.
	if got := canonicalQueryOf(q); got != q {
		t.Fatalf("canonicalQueryOf is not idempotent:\n got %q\nwant %q", got, q)
	}
}

// TestSignS3MatchesWireQuery recomputes the signature the way a compliant
// server does — over the request's actual query string — and requires it to
// equal the signature the client sent. Catches any divergence between the
// signed canonical query and the request on the wire.
func TestSignS3MatchesWireQuery(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut,
		"https://bucket.s3.amazonaws.com/path/obj.img.gz?partNumber=7&uploadId=AbC.x-y_z",
		nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	payload := emptyPayloadHash
	signS3(req, "AKIAEXAMPLE", "secret", "us-east-1", payload, now)

	auth := req.Header.Get("Authorization")
	i := strings.Index(auth, "Signature=")
	if i < 0 {
		t.Fatalf("no Signature in Authorization header: %q", auth)
	}
	clientSig := strings.TrimPrefix(auth[i:], "Signature=")

	serverSig := v4Signature("AKIAEXAMPLE", "secret", "us-east-1", "s3",
		req.Method, req.URL.EscapedPath(), canonicalQueryOf(req.URL.RawQuery),
		map[string]string{
			"host":                 req.URL.Host,
			"x-amz-content-sha256": payload,
			"x-amz-date":           req.Header.Get("x-amz-date"),
		},
		payload, now)
	if clientSig != serverSig {
		t.Fatalf("client signature %s does not match server-side recomputation %s", clientSig, serverSig)
	}
}

// TestRetryOnlyRetriesMarkedErrors: retry() must give up immediately on
// non-retryable errors and exhaust its attempts on retryable ones.
func TestRetryOnlyRetriesMarkedErrors(t *testing.T) {
	calls := 0
	err := retry(3, func() error {
		calls++
		return http.ErrBodyReadAfterClose // plain error: not retryable
	})
	if calls != 1 || err == nil {
		t.Fatalf("non-retryable error: %d calls, err=%v; want 1 call with error", calls, err)
	}

	calls = 0
	err = retry(3, func() error {
		calls++
		return &httpOpError{status: "503", retryable: true}
	})
	if calls != 3 || err == nil {
		t.Fatalf("retryable error: %d calls, err=%v; want 3 calls with error", calls, err)
	}

	calls = 0
	err = retry(3, func() error {
		calls++
		if calls < 2 {
			return &httpOpError{status: "500", retryable: true}
		}
		return nil
	})
	if calls != 2 || err != nil {
		t.Fatalf("retry-until-success: %d calls, err=%v; want 2 calls, nil", calls, err)
	}
}

// TestParseURLRejectsControlChars: percent-decoded credentials/paths must
// not smuggle CR/LF into wire protocols.
func TestParseURLRejectsControlChars(t *testing.T) {
	if _, err := ParseURL("ftp://user:p%0d%0aDELE%20x@host/file.img.gz"); err == nil {
		t.Fatal("ParseURL accepted CRLF injection in FTP password")
	}
	if _, err := ParseURL("sftp://u:p%0Ax@host/f.img.gz"); err == nil {
		t.Fatal("ParseURL accepted LF in SFTP password")
	}
	if _, err := ParseURL("ftp://u:p@host/a%0db.img.gz"); err == nil {
		t.Fatal("ParseURL accepted CR in FTP path")
	}
	// Normal URLs keep working.
	if _, err := ParseURL("sftp://user:p%40ss@host:22/dir/f.img.gz"); err != nil {
		t.Fatalf("ParseURL rejected a normal URL: %v", err)
	}
}
