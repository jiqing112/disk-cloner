package storage

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// openPixelDrain streams a file to pixeldrain.com (API:
// https://pixeldrain.com/api — PUT /api/file/{name} with the raw bytes as
// the request body, HTTP Basic auth where the password is the API key and
// the username is ignored; anonymous upload is not supported).
//
// On success the API answers 201 with {"success":true,"id":"..."}; the
// share page is pixeldrain.com/u/{id} (ShareURL reports it). Errors carry
// a stable machine value plus a human message, e.g. 413 file_too_large.
//
// The image is streamed with chunked transfer encoding so nothing touches
// the local disk. The API key is verified up front with GET /api/user/me:
// an early-rejected PUT can be lost to a TCP RST when the server closes
// the connection without draining the request body, and Go's http client
// does not deliver an early response while the body is still open — so
// auth is never trusted to the stream itself.
func openPixelDrain(cfg Config) (Writer, error) {
	if cfg.Password == "" {
		return nil, fmt.Errorf("pixeldrain: API key is required")
	}
	if cfg.Path == "" {
		return nil, fmt.Errorf("pixeldrain: file name is required")
	}
	name := strings.TrimPrefix(cfg.Path, "/")
	if name == "" {
		return nil, fmt.Errorf("pixeldrain: file name is required")
	}
	if len(name) > 255 {
		return nil, fmt.Errorf("pixeldrain: file name longer than 255 characters")
	}

	host := cfg.Host
	if host == "" {
		host = "pixeldrain.com"
	}
	scheme := "https"
	if !cfg.UseTLS {
		scheme = "http"
	}
	hostPort := host
	if (cfg.UseTLS && cfg.Port != 0 && cfg.Port != 443) || (!cfg.UseTLS && cfg.Port != 0 && cfg.Port != 80) {
		hostPort = net.JoinHostPort(host, strconv.Itoa(cfg.Port))
	}
	base := scheme + "://" + hostPort + "/api"
	endpoint := base + "/file/" + url.PathEscape(name)

	client := httpClient(cfg.InsecureTLS)

	// Preflight: verify the API key before any image data flows — a bad
	// key must fail here (and fail fast, before the hours-long zero-fill),
	// not hours into the transfer.
	{
		req, err := http.NewRequest(http.MethodGet, base+"/user/me", nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth("", cfg.Password)
		preErr := retry(3, func() error {
			resp, err := client.Do(req)
			if err != nil {
				return &httpOpError{status: "transport error", detail: err.Error(), retryable: true}
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return nil
			}
			if resp.StatusCode == http.StatusUnauthorized {
				return fmt.Errorf("认证失败: API key 无效或已过期 (pixeldrain.com 账户设置页可生成): %s", pdMessage(body, resp.Status))
			}
			return &httpOpError{status: resp.Status, detail: pdMessage(body, resp.Status), retryable: resp.StatusCode >= 500}
		})
		if preErr != nil {
			return nil, fmt.Errorf("pixeldrain: %s", preErr.Error())
		}
	}

	pr, pw := io.Pipe()
	w := &pixeldrainWriter{client: client, endpoint: endpoint, key: cfg.Password, pw: pw, done: make(chan pdResult, 1)}
	go func() {
		req, err := http.NewRequest(http.MethodPut, endpoint, pr)
		if err != nil {
			w.done <- pdResult{err: err}
			pw.Close()
			return
		}
		req.SetBasicAuth("", w.key) // username ignored by the API
		req.Header.Set("Content-Type", "application/octet-stream")
		w.putAndRead(req)
		pw.Close()
	}()
	// The server only answers after the whole body is consumed, so done
	// fires at Close time; a completion within this window means something
	// is wrong with the stream setup — refuse a dead writer.
	select {
	case res := <-w.done:
		if res.err != nil {
			return nil, res.err
		}
		return nil, fmt.Errorf("pixeldrain: upload finished before any data was written")
	case <-time.After(3 * time.Second):
	}
	return w, nil
}

type pdResult struct {
	err error
	id  string
}

// pdMessage extracts the human message from a pixeldrain JSON reply
// ({"success":false,"value":..,"message":..}), falling back to the HTTP
// status text.
func pdMessage(body []byte, fallback string) string {
	var pd struct {
		Value   string `json:"value"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &pd)
	if pd.Message != "" {
		return pd.Message
	}
	if pd.Value != "" {
		return pd.Value
	}
	return fallback
}

type pixeldrainWriter struct {
	client   *http.Client
	endpoint string
	key      string

	pw   *io.PipeWriter
	done chan pdResult

	shareURL string

	mu       sync.Mutex
	finished bool
}

func (w *pixeldrainWriter) Write(p []byte) (int, error) {
	return w.pw.Write(p)
}

// ShareURL returns the pixeldrain share page (pixeldrain.com/u/{id}) after
// a successful Close.
func (w *pixeldrainWriter) ShareURL() string { return w.shareURL }

func (w *pixeldrainWriter) Close() error { return w.finish(false) }

// Abort discards the upload. pixeldrain only creates the file once the
// request body completed, so an aborted stream leaves nothing to delete.
func (w *pixeldrainWriter) Abort() error {
	w.finish(true)
	return nil
}

func (w *pixeldrainWriter) finish(abandon bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return nil
	}
	w.finished = true

	if abandon {
		w.pw.CloseWithError(io.ErrClosedPipe)
		// Wait so the goroutine is done before the connection drops.
		select {
		case <-w.done:
		case <-time.After(5 * time.Second):
		}
		return nil
	}

	w.pw.Close() // EOF → server finalizes and answers
	var res pdResult
	select {
	case res = <-w.done:
	case <-time.After(finalizeWait):
		return fmt.Errorf("pixeldrain: no response from server within %s (upload outcome unknown — verify the file in your pixeldrain account)", finalizeWait)
	}
	if res.err != nil {
		return res.err
	}
	if res.id != "" {
		w.shareURL = "https://pixeldrain.com/u/" + url.PathEscape(res.id)
	}
	return nil
}

// putAndRead performs the streaming PUT and interprets the pixeldrain JSON
// reply. Runs on the upload goroutine; sends exactly one pdResult.
func (w *pixeldrainWriter) putAndRead(req *http.Request) {
	resp, err := w.client.Do(req)
	if err != nil {
		w.done <- pdResult{err: &httpOpError{status: "transport error", detail: err.Error(), retryable: true}}
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()

	var pd struct {
		Success bool   `json:"success"`
		ID      string `json:"id"`
	}
	_ = json.Unmarshal(body, &pd)

	if resp.StatusCode == http.StatusCreated && pd.Success && pd.ID != "" {
		w.done <- pdResult{id: pd.ID}
		return
	}
	msg := pdMessage(body, resp.Status)
	switch resp.StatusCode {
	case http.StatusRequestEntityTooLarge:
		msg = "file exceeds the pixeldrain size limit: " + msg
	case http.StatusLengthRequired:
		msg = "server rejected chunked upload (411) — pixeldrain streaming is unavailable on this endpoint: " + msg
	}
	w.done <- pdResult{err: &httpOpError{
		status:    resp.Status,
		detail:    msg,
		retryable: resp.StatusCode >= 500,
	}}
}
