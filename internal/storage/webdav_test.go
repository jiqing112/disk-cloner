package storage

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWebDAV records PUT/DELETE/MKCOL requests and can be told to reject
// chunked bodies (simulating nginx dav_module's 411 Length Required).
type fakeWebDAV struct {
	mu            sync.Mutex
	bodies        map[string][]byte
	contentLens   map[string]int64
	chunked       map[string]bool
	deleted       []string
	mkcols        []string
	rejectChunked bool
}

func (f *fakeWebDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		isChunked := len(r.TransferEncoding) > 0 && r.TransferEncoding[0] == "chunked"
		if isChunked && f.rejectChunked {
			w.WriteHeader(http.StatusLengthRequired)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if f.bodies == nil {
			f.bodies = map[string][]byte{}
			f.contentLens = map[string]int64{}
			f.chunked = map[string]bool{}
		}
		f.bodies[r.URL.Path] = body
		f.contentLens[r.URL.Path] = r.ContentLength
		f.chunked[r.URL.Path] = isChunked
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		f.deleted = append(f.deleted, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	case "MKCOL":
		f.mkcols = append(f.mkcols, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed) // pretend it exists
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func TestWebDAVStreamingUpload(t *testing.T) {
	srv := &fakeWebDAV{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := Config{Kind: KindWebDAV, URL: ts.URL + "/dir/file.img.gz", User: "u", Password: "p", InsecureTLS: true}
	w, err := openWebDAV(cfg)
	if err != nil {
		t.Fatalf("openWebDAV: %v", err)
	}
	payload := bytes.Repeat([]byte{0x5A}, 1<<20)
	if n, err := w.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if got := srv.bodies["/dir/file.img.gz"]; !bytes.Equal(got, payload) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if !srv.chunked["/dir/file.img.gz"] {
		t.Error("expected the upload to be streamed chunked")
	}
	if len(srv.mkcols) == 0 || srv.mkcols[len(srv.mkcols)-1] != "/dir" {
		t.Errorf("MKCOL parents = %v", srv.mkcols)
	}
}

func TestWebDAVSpoolFallbackOn411(t *testing.T) {
	srv := &fakeWebDAV{rejectChunked: true}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := Config{Kind: KindWebDAV, URL: ts.URL + "/file.img.gz", User: "u", Password: "p", InsecureTLS: true}
	w, err := openWebDAV(cfg)
	if err != nil {
		t.Fatalf("openWebDAV: %v", err)
	}
	payload := bytes.Repeat([]byte{0x01}, 256*1024)
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if got := srv.bodies["/file.img.gz"]; !bytes.Equal(got, payload) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if srv.chunked["/file.img.gz"] {
		t.Error("fallback upload must carry an explicit Content-Length, not chunked")
	}
	if srv.contentLens["/file.img.gz"] != int64(len(payload)) {
		t.Errorf("Content-Length = %d, want %d", srv.contentLens["/file.img.gz"], len(payload))
	}
}

func TestWebDAVAbortDeletesPartial(t *testing.T) {
	srv := &fakeWebDAV{}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	cfg := Config{Kind: KindWebDAV, URL: ts.URL + "/file.img.gz", User: "u", Password: "p", InsecureTLS: true}
	w, err := openWebDAV(cfg)
	if err != nil {
		t.Fatalf("openWebDAV: %v", err)
	}
	w.Write([]byte("partial"))
	w.Abort()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		found := false
		for _, d := range srv.deleted {
			if d == "/file.img.gz" {
				found = true
			}
		}
		srv.mu.Unlock()
		if found {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("partial file was never DELETEd after Abort")
}

// TestWebDAVCloseCleansUpOnError ensures a failing server side (500) leaves
// no partial remote file behind.
func TestWebDAVCloseCleansUpOnError(t *testing.T) {
	srv := &fakeWebDAV{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "boom")
			return
		}
		srv.ServeHTTP(w, r)
	}))
	defer ts.Close()

	cfg := Config{Kind: KindWebDAV, URL: ts.URL + "/file.img.gz", User: "u", Password: "p", InsecureTLS: true}
	w, err := openWebDAV(cfg)
	if err != nil {
		t.Fatalf("openWebDAV: %v", err)
	}
	// The streaming probe already succeeded above; this server 500s on the
	// real PUT. Writes may fail (broken pipe) or Close surfaces the 500.
	w.Write(bytes.Repeat([]byte{0x02}, 4096))
	if err := w.Close(); err == nil {
		t.Fatal("Close = nil error, want server error")
	} else if !strings.Contains(err.Error(), "500") && !strings.Contains(err.Error(), "broken pipe") &&
		!strings.Contains(err.Error(), "EOF") {
		t.Logf("Close error: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.deleted) == 0 {
		t.Fatal("partial file was never DELETEd after failed Close")
	}
}
