package storage

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// errWebDAVClosed is returned by Write after Close/Abort — every other
// backend returns an error for this misuse, and the spool writer used to
// nil-panic instead (finish() clears the spool file but Write fell through
// to the unset pipe writer).
var errWebDAVClosed = errors.New("webdav: writer already closed")

// openWebDAV uploads via HTTP PUT. Most WebDAV servers accept chunked
// uploads (which allow true streaming), but some (e.g. nginx dav_module)
// require Content-Length — for those we probe first and fall back to
// spooling through a local temp file. MKCOL is attempted for missing parent
// collections (best effort).
func openWebDAV(cfg Config) (Writer, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("webdav: URL is required")
	}
	if !strings.HasPrefix(cfg.URL, "http://") && !strings.HasPrefix(cfg.URL, "https://") {
		return nil, fmt.Errorf("webdav: URL must start with http:// or https://")
	}
	client := httpClient(cfg.InsecureTLS)
	webdavMkcolParents(client, cfg.URL, cfg.User, cfg.Password)

	chunked, err := webdavProbeChunked(client, cfg.URL, cfg.User, cfg.Password)
	if err != nil {
		return nil, err
	}
	if chunked {
		return webdavStreamWriter(client, cfg)
	}
	if cfg.Logf != nil {
		cfg.Logf("[!] WebDAV 服务器不支持分块上传,镜像将先写入本地临时文件再上传 — 请确保本地有足够的空闲空间(内存系统上即内存)")
	}
	return webdavSpoolWriter(client, cfg)
}

// webdavProbeChunked checks whether the server accepts a chunked PUT by
// writing (and deleting) a tiny probe file. chunked=false is returned ONLY
// for the statuses that genuinely mean "chunked bodies not supported"
// (411 Length Required / 501), which trigger the Content-Length spool
// fallback. Any other failure — bad credentials (401), forbidden (403),
// missing parent (409), server error (5xx), unreachable host — is returned
// as an error so Open fails fast: silently falling back to spool mode here
// used to buffer the entire image into a local temp file (RAM on tmpfs
// systems) only to die hours later with the same error.
func webdavProbeChunked(client *http.Client, fileURL, user, pass string) (bool, error) {
	probe := fileURL + ".diskcloner-probe"
	// io.NopCloser hides the concrete reader type from http.NewRequest, so
	// ContentLength stays 0 and the request goes out with chunked encoding.
	body := io.NopCloser(strings.NewReader("ok"))
	req, err := http.NewRequest(http.MethodPut, probe, body)
	if err != nil {
		return false, err
	}
	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("webdav: 探测失败 (无法连接 %s): %w", fileURL, err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// chunked upload accepted
	case resp.StatusCode == http.StatusLengthRequired || resp.StatusCode == http.StatusNotImplemented:
		// Server requires a known length (nginx dav_module and friends) —
		// the spool fallback exists for exactly this case.
		return false, nil
	default:
		return false, fmt.Errorf("webdav: 服务器拒绝了上传探测: %s (请检查认证信息/路径/服务器状态)", resp.Status)
	}

	if dreq, err := http.NewRequest(http.MethodDelete, probe, nil); err == nil {
		dreq.SetBasicAuth(user, pass)
		if dresp, err := client.Do(dreq); err == nil {
			io.Copy(io.Discard, io.LimitReader(dresp.Body, 4096))
			dresp.Body.Close()
		}
	}
	return true, nil
}

// webdavMkcolParents creates parent collections of fileURL one level at a
// time, ignoring errors (405 = already exists etc.).
func webdavMkcolParents(client *http.Client, fileURL, user, pass string) {
	u, err := url.Parse(fileURL)
	if err != nil || u.Path == "" {
		return
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) <= 1 {
		return // file sits in the root collection
	}
	prefix := ""
	for _, s := range segs[:len(segs)-1] {
		if s == "" {
			continue
		}
		prefix += "/" + s
		cu := *u
		cu.Path = prefix
		req, err := http.NewRequest("MKCOL", cu.String(), nil)
		if err != nil {
			return
		}
		req.SetBasicAuth(user, pass)
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
		}
	}
}

// webdavStreamWriter pipes writes straight into a chunked PUT.
func webdavStreamWriter(client *http.Client, cfg Config) (Writer, error) {
	pr, pw := io.Pipe()
	w := &webdavWriter{
		client: client, cfg: cfg, pw: pw,
		done: make(chan error, 1),
	}
	go func() {
		req, err := http.NewRequest(http.MethodPut, cfg.URL, pr)
		if err != nil {
			w.done <- err
			pw.Close()
			return
		}
		req.SetBasicAuth(cfg.User, cfg.Password)
		req.Header.Set("Content-Type", "application/octet-stream")
		doErr := webdavDo(client, req, cfg.URL)
		if doErr != nil {
			doErr = fmt.Errorf("webdav: PUT %s: %w", cfg.URL, doErr)
		}
		w.done <- doErr
		pw.Close()
	}()
	// Fail fast on an early rejection from the server.
	select {
	case err := <-w.done:
		if err != nil {
			return nil, err
		}
		// done delivered a nil error inside the probe window: the transfer
		// somehow completed already — treat as unusable rather than handing
		// back a dead writer that would panic on first Write.
		return nil, fmt.Errorf("webdav: upload finished before any data was written")
	case <-time.After(3 * time.Second):
	}
	return w, nil
}

// webdavSpoolWriter spools through a local temp file and PUTs it with an
// explicit Content-Length on Close (for servers that reject chunked bodies).
func webdavSpoolWriter(client *http.Client, cfg Config) (Writer, error) {
	tmp, err := os.CreateTemp("", "diskcloner-dav-")
	if err != nil {
		return nil, fmt.Errorf("webdav: local spool file (server requires Content-Length): %w", err)
	}
	return &webdavWriter{client: client, cfg: cfg, spool: tmp}, nil
}

func webdavDo(client *http.Client, req *http.Request, what string) error {
	resp, err := client.Do(req)
	if err != nil {
		return &httpOpError{status: "transport error", detail: err.Error(), retryable: true}
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpOpError{
			status:    resp.Status,
			retryable: resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout,
		}
	}
	return nil
}

type webdavWriter struct {
	client *http.Client
	cfg    Config

	// streaming mode
	pw   *io.PipeWriter
	done chan error

	// spool mode
	spool *os.File
	size  int64

	mu       sync.Mutex
	finished bool
}

func (w *webdavWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return 0, errWebDAVClosed
	}
	if w.spool != nil {
		n, err := w.spool.Write(p)
		w.size += int64(n)
		return n, err
	}
	return w.pw.Write(p)
}

// Close finalizes the upload. On error the partial remote file is DELETEd
// and the spool temp file (if any) removed.
func (w *webdavWriter) Close() error {
	return w.finish(false)
}

// Abort discards the upload and removes the partial remote file.
func (w *webdavWriter) Abort() error {
	w.finish(true)
	return nil
}

func (w *webdavWriter) finish(abandon bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return nil
	}
	w.finished = true

	var err error
	outcomeUnknown := false
	if w.spool != nil {
		spoolPath := w.spool.Name()
		spoolSize := w.size
		w.spool.Close()
		w.spool = nil
		if !abandon {
			// The body is a local file — reopen per attempt so the request
			// is replayable on transient failures.
			err = retry(3, func() error {
				return w.putSpoolFile(spoolPath, spoolSize)
			})
		}
		os.Remove(spoolPath)
	} else {
		if abandon {
			w.pw.CloseWithError(io.ErrClosedPipe)
		} else {
			w.pw.Close()
		}
		select {
		case err = <-w.done:
		case <-time.After(finalizeWait):
			outcomeUnknown = true
			err = fmt.Errorf("webdav: no response from server within %s", finalizeWait)
		}
	}

	// Delete the partial remote file only when its partial-ness is certain:
	// an explicit abort or a transfer rejected by the server. An unknown
	// outcome (server still ingesting after the stream ended) must NOT be
	// deleted — the file may already be complete.
	if (abandon || (err != nil && !outcomeUnknown)) && w.cfg.URL != "" {
		w.webdavDeleteRemote()
	}
	if err != nil && outcomeUnknown {
		err = fmt.Errorf("%w (file kept on server — verify it before trusting it)", err)
	}
	return err
}

func (w *webdavWriter) putSpoolFile(spoolPath string, size int64) error {
	f, openErr := os.Open(spoolPath)
	if openErr != nil {
		return fmt.Errorf("webdav: reopen spool file: %w", openErr)
	}
	defer f.Close()
	req, reqErr := http.NewRequest(http.MethodPut, w.cfg.URL, f)
	if reqErr != nil {
		return reqErr
	}
	req.ContentLength = size
	req.SetBasicAuth(w.cfg.User, w.cfg.Password)
	req.Header.Set("Content-Type", "application/octet-stream")
	err := webdavDo(w.client, req, w.cfg.URL)
	if err != nil {
		return fmt.Errorf("webdav: PUT %s: %w", w.cfg.URL, err)
	}
	return nil
}

func (w *webdavWriter) webdavDeleteRemote() {
	if dreq, derr := http.NewRequest(http.MethodDelete, w.cfg.URL, nil); derr == nil {
		dreq.SetBasicAuth(w.cfg.User, w.cfg.Password)
		if dresp, derr := w.client.Do(dreq); derr == nil {
			io.Copy(io.Discard, io.LimitReader(dresp.Body, 4096))
			dresp.Body.Close()
		}
	}
}
