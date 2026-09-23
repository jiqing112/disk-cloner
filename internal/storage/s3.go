package storage

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// openS3 streams to S3-compatible object storage (AWS S3, MinIO, Ceph, ...)
// using a hand-rolled SigV4 + multipart upload — no SDK, so the tool keeps
// zero heavy dependencies. The writer buffers one part at a time (starting
// at 8 MiB, growing to 256 MiB for very large images) and completes the
// multipart upload on Close.
func openS3(cfg Config) (Writer, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("s3: endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3: bucket is required")
	}
	if cfg.Key == "" {
		return nil, fmt.Errorf("s3: object key is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("s3: access key and secret key are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}

	w := &s3Writer{
		cfg:      cfg,
		client:   httpClient(cfg.InsecureTLS),
		key:      cfg.Key,
		partSize: 8 << 20, // 8 MiB, grows every 1000 parts (cap 256 MiB)
		buf:      make([]byte, 0, 8<<20),
	}
	// Fail fast: CreateMultipartUpload validates credentials, bucket and
	// permissions before any disk data is transferred.
	id, err := w.createMultipart()
	if err != nil {
		return nil, err
	}
	w.uploadID = id
	return w, nil
}

type s3Writer struct {
	cfg    Config
	client *http.Client
	key    string // raw object key (no bucket, no leading slash); encoded once when building request URLs

	uploadID  string
	etags     []string
	partSize  int
	partNum   int
	buf       []byte
	err       error
	finalized bool
}

func (w *s3Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	total := 0
	for len(p) > 0 {
		space := w.partSize - len(w.buf)
		n := len(p)
		if n > space {
			n = space
		}
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		total += n
		if len(w.buf) == w.partSize {
			if err := w.uploadPart(); err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

// Close flushes the final part and completes the multipart upload. On any
// error the multipart upload is aborted so no partial object is left behind.
func (w *s3Writer) Close() error {
	if w.err != nil {
		w.abort()
		return w.err
	}
	if w.finalized {
		return nil
	}
	if len(w.buf) > 0 || w.partNum == 0 {
		if err := w.uploadPart(); err != nil {
			w.abort()
			return err
		}
	}
	if err := w.complete(); err != nil {
		w.abort()
		return err
	}
	w.finalized = true
	return nil
}

// Abort discards the multipart upload and any uploaded parts.
func (w *s3Writer) Abort() error {
	w.abort()
	return nil
}

func (w *s3Writer) uploadPart() error {
	if len(w.buf) == 0 && w.partNum > 0 {
		return nil // no trailing empty part
	}
	body := w.buf
	partNumber := w.partNum + 1
	// The body is a fully buffered slice, so the request is replayable —
	// retry transient transport/server failures instead of aborting hours
	// of transfer on one hiccup.
	var etag string
	err := retry(3, func() error {
		var err error
		etag, err = w.doUploadPart(partNumber, body)
		return err
	})
	if err != nil {
		if serr, ok := err.(*httpOpError); ok {
			w.err = fmt.Errorf("s3: upload part %d: %s %s", partNumber, serr.status, serr.detail)
		} else {
			w.err = fmt.Errorf("s3: upload part %d: %w", partNumber, err)
		}
		return w.err
	}
	w.etags = append(w.etags, etag)
	w.partNum = partNumber
	w.buf = w.buf[:0]
	// Grow parts for very large images: 1000 parts per size step keeps the
	// total under S3's 10000-part limit for ~1.5+ TiB objects.
	if w.partNum%1000 == 0 && w.partSize < 256<<20 {
		w.partSize *= 2
	}
	return nil
}

// httpOpError carries the HTTP status and server message for a failed
// request so the retry wrapper can decide and the caller can format.
type httpOpError struct {
	status    string
	detail    string
	retryable bool
}

func (e *httpOpError) Error() string { return e.status + " " + e.detail }

func (w *s3Writer) doUploadPart(partNumber int, body []byte) (string, error) {
	req, err := w.s3req(http.MethodPut, w.objectPath(), s3Query([2]string{"partNumber", fmt.Sprint(partNumber)}, [2]string{"uploadId", w.uploadID}), body)
	if err != nil {
		return "", err
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return "", &httpOpError{status: "transport error", detail: err.Error(), retryable: true}
	}
	// Read the body (bounded) before closing so error details survive.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", &httpOpError{
			status:    resp.Status,
			detail:    s3ErrorMessageBytes(respBody),
			retryable: resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout,
		}
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", &httpOpError{status: "response", detail: "no ETag in response", retryable: true}
	}
	return etag, nil
}

func (w *s3Writer) complete() error {
	var b bytes.Buffer
	b.WriteString("<CompleteMultipartUpload>")
	for i, etag := range w.etags {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, etag)
	}
	b.WriteString("</CompleteMultipartUpload>")
	body := b.Bytes()

	// Complete is idempotent per upload ID and the body is buffered — safe
	// to retry on transient failures (S3 may 500 while assembling parts).
	// A transport error here is marked retryable too: without it, one
	// dropped connection at finalize time aborted the whole multipart
	// upload and destroyed hours of transfer.
	if err := retry(3, func() error {
		req, err := w.s3req(http.MethodPost, w.objectPath(), s3Query([2]string{"uploadId", w.uploadID}), body)
		if err != nil {
			return err
		}
		resp, err := w.client.Do(req)
		if err != nil {
			return &httpOpError{status: "transport error", detail: fmt.Sprintf("complete upload: %v", err), retryable: true}
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		// A 200 can still carry an XML error body (S3 quirk) — check both.
		if resp.StatusCode/100 != 2 || bytes.Contains(respBody, []byte("<Error>")) {
			// 404 NoSuchUpload after a transport-error retry usually means
			// the FIRST complete succeeded server-side but its response
			// was lost. Verify with a HEAD instead of failing hours of
			// transfer over a finished upload.
			if resp.StatusCode == http.StatusNotFound && bytes.Contains(respBody, []byte("NoSuchUpload")) && w.objectExists() {
				return nil
			}
			return &httpOpError{
				status:    resp.Status,
				detail:    s3ErrorMessageBytes(respBody),
				retryable: resp.StatusCode >= 500,
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("s3: complete upload: %w", err)
	}
	return nil
}

// objectExists reports whether the target object is already present — used
// to disambiguate NoSuchUpload on CompleteMultipartUpload (the complete may
// have succeeded server-side while its response was lost in transit).
func (w *s3Writer) objectExists() bool {
	req, err := w.s3req(http.MethodHead, w.objectPath(), "", nil)
	if err != nil {
		return false
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	return resp.StatusCode/100 == 2
}

func (w *s3Writer) createMultipart() (string, error) {
	var uploadID string
	err := retry(3, func() error {
		id, err := w.doCreateMultipart()
		if err != nil {
			return err
		}
		uploadID = id
		return nil
	})
	return uploadID, err
}

func (w *s3Writer) doCreateMultipart() (string, error) {
	req, err := w.s3req(http.MethodPost, w.objectPath(), s3Query([2]string{"uploads", ""}), nil)
	if err != nil {
		return "", err
	}
	resp, err := w.client.Do(req)
	if err != nil {
		// Transport errors are transient (connection reuse races, RSTs) —
		// retryable so Open doesn't fail on a one-off hiccup.
		return "", &httpOpError{status: "transport error", detail: fmt.Sprintf("create multipart upload: %v", err), retryable: true}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", &httpOpError{
			status:    resp.Status,
			detail:    s3ErrorMessageBytes(body),
			retryable: resp.StatusCode >= 500,
		}
	}
	var parsed struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(body, &parsed); err != nil || parsed.UploadID == "" {
		return "", fmt.Errorf("s3: create multipart upload: no UploadId in response")
	}
	return parsed.UploadID, nil
}

func (w *s3Writer) abort() {
	if w.uploadID == "" || w.finalized {
		return
	}
	w.finalized = true
	req, err := w.s3req(http.MethodDelete, w.objectPath(), s3Query([2]string{"uploadId", w.uploadID}), nil)
	if err != nil {
		return
	}
	if resp, err := w.client.Do(req); err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}
}

// objectPath returns the raw (unencoded) request path — bucket included
// only for path-style addressing. The percent-encoding happens exactly once,
// in s3req's RawPath.
func (w *s3Writer) objectPath() string {
	if w.cfg.PathStyle {
		return "/" + w.cfg.Bucket + "/" + w.cfg.Key
	}
	return "/" + w.cfg.Key
}

// s3req builds a signed request. rawQuery must already be in canonical
// (sorted, AWS-encoded) form — s3Query produces that.
func (w *s3Writer) s3req(method, pathForURL, rawQuery string, body []byte) (*http.Request, error) {
	scheme := "https"
	if !w.cfg.UseTLS {
		scheme = "http"
	}
	host := w.cfg.Endpoint
	if !w.cfg.PathStyle {
		host = w.cfg.Bucket + "." + host
	}
	u := &url.URL{Scheme: scheme, Host: host, Path: pathForURL, RawQuery: rawQuery}
	// pathForURL carries the RAW key; encode it once here for the wire.
	// (Encoding an already-encoded key escaped the '%' signs a second time
	// and uploaded objects under mojibake names for keys with spaces or
	// non-ASCII characters.)
	u.RawPath = awsURIEncode(pathForURL, false)

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u.String(), rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	payloadHash := sha256Hex(body)
	signS3(req, w.cfg.AccessKey, w.cfg.SecretKey, w.cfg.Region, payloadHash, time.Now())
	return req, nil
}

func s3Query(pairs ...[2]string) string {
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	var b strings.Builder
	for i, kv := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(kv[0])
		b.WriteByte('=')
		b.WriteString(awsURIEncode(kv[1], true))
	}
	return b.String()
}

// ── SigV4 ────────────────────────────────────────────────────────────────

const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func sha256Hex(b []byte) string {
	if b == nil {
		return emptyPayloadHash
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// awsURIEncode implements the AWS variant of percent-encoding: unreserved
// characters (ALPHA / DIGIT / - . _ ~) stay literal, everything else is
// %XX-escaped; '/' is preserved when encodeSlash is false (paths).
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// v4Signature computes the SigV4 signature hex for a request. headers must
// contain every header that participates in signing (lowercase keys); the
// signed header list is derived from it, sorted.
func v4Signature(access, secret, region, service, method, canonicalURI, canonicalQuery string,
	headers map[string]string, payloadHash string, now time.Time) string {

	amzDate := now.UTC().Format("20060102T150405Z")
	date := now.UTC().Format("20060102")

	names := make([]string, 0, len(headers))
	canonicalHeaders := ""
	for k := range headers {
		lk := strings.ToLower(k)
		names = append(names, lk)
	}
	sort.Strings(names)
	for _, k := range names {
		canonicalHeaders += k + ":" + strings.TrimSpace(headers[k]) + "\n"
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := date + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	return hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))
}

// signS3 signs req in place: sets x-amz-date, x-amz-content-sha256 and the
// Authorization header. host + x-amz-* are the signed headers.
func signS3(req *http.Request, access, secret, region, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	headers := map[string]string{
		"host": req.URL.Host,
	}
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") && len(vs) > 0 {
			headers[lk] = vs[0]
		}
	}
	canonicalURI := req.URL.EscapedPath()
	canonicalQuery := canonicalQueryOf(req.URL.RawQuery)
	sig := v4Signature(access, secret, region, "s3", req.Method, canonicalURI, canonicalQuery, headers, payloadHash, now)
	scope := now.UTC().Format("20060102") + "/" + region + "/s3/aws4_request"

	var names []string
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		access, scope, strings.Join(names, ";"), sig))
}

// canonicalQueryOf normalizes a raw query string into canonical form
// (sorted by key, AWS-encoded values). Query keys keep their original case:
// SigV4 requires the canonical query to match the request byte-for-byte
// (lowercasing breaks partNumber/uploadId on AWS and MinIO). Only used with
// queries built by s3Query, but kept strict in case of future extras.
func canonicalQueryOf(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	pairs := [][2]string{}
	for _, kv := range strings.Split(rawQuery, "&") {
		k, v, _ := strings.Cut(kv, "=")
		decodedV, err := url.QueryUnescape(v)
		if err != nil {
			decodedV = v
		}
		pairs = append(pairs, [2]string{k, decodedV})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	var b strings.Builder
	for i, kv := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(kv[0])
		b.WriteByte('=')
		b.WriteString(awsURIEncode(kv[1], true))
	}
	return b.String()
}

func s3ErrorMessageBytes(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if err := xml.Unmarshal(body, &e); err != nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return ""
}
