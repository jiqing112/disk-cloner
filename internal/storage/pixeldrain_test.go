package storage

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestParseURLPixelDrain covers the pixeldrain:// and pd:// schemes: API
// key normalization (password position or bare user), default host, port,
// tls=0 and the missing-key error.
func TestParseURLPixelDrain(t *testing.T) {
	cfg, err := ParseURL("pixeldrain://:mykey@pixeldrain.com/backup/disk.img.gz")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	if cfg.Kind != KindPixelDrain || cfg.Password != "mykey" || cfg.User != "" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.Host != "pixeldrain.com" || cfg.Path != "backup/disk.img.gz" || cfg.Port != 443 || !cfg.UseTLS {
		t.Fatalf("unexpected config: %+v", cfg)
	}

	cfg, err = ParseURL("pd://mykey@127.0.0.1:8081/disk.img.gz?tls=0")
	if err != nil {
		t.Fatalf("ParseURL pd alias: %v", err)
	}
	if cfg.Password != "mykey" || cfg.Host != "127.0.0.1" || cfg.Port != 8081 || cfg.UseTLS {
		t.Fatalf("unexpected config: %+v", cfg)
	}

	if _, err = ParseURL("pd://pixeldrain.com/disk.img.gz"); err == nil ||
		!strings.Contains(err.Error(), "API key") {
		t.Fatalf("missing key: err = %v, want API key error", err)
	}
}

// pdFake implements the pixeldrain API surface used by the writer:
// GET /api/user/me (key preflight) and PUT /api/file/{name} with Basic
// auth (password = key) and a JSON reply.
type pdFake struct {
	mu       sync.Mutex
	body     []byte
	sawName  string
	reply    int
	replyXML string
	wantKey  string
	// rejectChunked: answer 411 to PUTs without a Content-Length
	rejectChunked bool
}

func (f *pdFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 注意:读取 body 期间绝不能持有 f.mu——body 的 EOF 依赖客户端写端
	// 关闭,而客户端又在等服务端响应,持锁读会互相等待造成死锁。
	_, pass, ok := r.BasicAuth()
	if !ok || pass != f.wantKey {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false, "value": "authentication_required", "message": "you need to be authenticated",
		})
		return
	}

	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/user/me") {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": "user1"})
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/api/file/")
	if f.rejectChunked && r.ContentLength <= 0 {
		w.WriteHeader(http.StatusLengthRequired)
		return
	}
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.body = body
	f.sawName = name
	reply, replyXML := f.reply, f.replyXML
	f.mu.Unlock()

	if reply != 0 {
		w.WriteHeader(reply)
		json.NewEncoder(w).Encode(json.RawMessage(replyXML))
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "id": "abc123", "name": name, "size": len(body),
	})
}

// pdCfg builds a Config from an httptest server URL (strips the scheme —
// the pixeldrain backend re-adds http/https itself).
func pdCfg(ts *httptest.Server, name string) Config {
	host, port := splitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	return Config{Kind: KindPixelDrain, Host: host, Port: port, Password: "KEY", Path: name}
}

// TestPixelDrainUploadEndToEnd: key preflight + streaming chunked PUT,
// correct auth header, 201 reply parsed, share URL built.
func TestPixelDrainUploadEndToEnd(t *testing.T) {
	f := &pdFake{wantKey: "KEY"}
	ts := httptest.NewServer(f)
	defer ts.Close()

	w, err := Open(pdCfg(ts, "disk.img.gz"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	payload := bytes.Repeat([]byte("PIXEL"), 60000) // 300 KB, several writes
	if n, err := w.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !bytes.Equal(f.body, payload) {
		t.Fatalf("uploaded %d bytes, want %d", len(f.body), len(payload))
	}
	if f.sawName != "disk.img.gz" {
		t.Fatalf("file name = %q", f.sawName)
	}
	linker, ok := w.(interface{ ShareURL() string })
	if !ok || linker.ShareURL() != "https://pixeldrain.com/u/abc123" {
		t.Fatalf("ShareURL = %v", linker)
	}
}

// TestPixelDrainAuthFailFast: a wrong API key must abort during the
// preflight (before any image data flows), with the API's error message.
func TestPixelDrainAuthFailFast(t *testing.T) {
	f := &pdFake{wantKey: "OTHER"}
	ts := httptest.NewServer(f)
	defer ts.Close()

	start := time.Now()
	_, err := Open(pdCfg(ts, "x.img.gz"))
	if err == nil || !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("Open err = %v, want auth failure", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("auth failure was not detected during preflight")
	}
}

// TestPixelDrainErrorMessage: non-201 replies surface the API message and
// the 413 size-limit case is labeled.
func TestPixelDrainErrorMessage(t *testing.T) {
	f := &pdFake{wantKey: "KEY", reply: 413,
		replyXML: `{"success":false,"value":"file_too_large","message":"the file is too large"}`}
	ts := httptest.NewServer(f)
	defer ts.Close()

	w, err := Open(pdCfg(ts, "x.img.gz"))
	if err != nil {
		// rejected in the fail-fast window — also acceptable
		if !strings.Contains(err.Error(), "pixeldrain size limit") {
			t.Fatalf("Open err = %v", err)
		}
		return
	}
	w.Write([]byte("data"))
	err = w.Close()
	if err == nil || !strings.Contains(err.Error(), "file is too large") {
		t.Fatalf("Close err = %v, want size-limit message", err)
	}
}

// TestPixelDrainChunkedRejected: a server that rejects chunked bodies with
// 411 must produce a clear error at Close (the stream cannot be replayed
// at that point; real pixeldrain accepts chunked uploads).
func TestPixelDrainChunkedRejected(t *testing.T) {
	f := &pdFake{wantKey: "KEY", rejectChunked: true}
	ts := httptest.NewServer(f)
	defer ts.Close()

	w, err := Open(pdCfg(ts, "disk.img.gz"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := w.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	err = w.Close()
	if err == nil || !strings.Contains(err.Error(), "411") {
		t.Fatalf("Close err = %v, want 411 chunked-reject error", err)
	}
}
