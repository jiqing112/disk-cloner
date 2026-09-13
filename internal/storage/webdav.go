package storage

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

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

	if webdavProbeChunked(client, cfg.URL, cfg.User, cfg.Password) {
		return webdavStreamWriter(client, cfg)
	}
	return webdavSpoolWriter(client, cfg)
}

// webdavProbeChunked checks whether the server accepts a chunked PUT by
// writing (and deleting) a tiny probe file. Returns false when the server
// rejects chunked bodies (411/501) or drops the connection.
func webdavProbeChunked(client *http.Client, fileURL, user, pass string) bool {
	probe := fileURL + ".diskcloner-probe"
	// io.NopCloser hides the concrete reader type from http.NewRequest, so
	// ContentLength stays 0 and the request goes out with chunked encoding.
	body := io.NopCloser(strings.NewReader("ok"))
	req, err := http.NewRequest(http.MethodPut, probe, body)
	if err != nil {
		return false
	}
	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300

	if dreq, err := http.NewRequest(http.MethodDelete, probe, nil); err == nil {
		dreq.SetBasicAuth(user, pass)
		if dresp, err := client.Do(dreq); err == nil {
			io.Copy(io.Discard, io.LimitReader(dresp.Body, 4096))
			dresp.Body.Close()
		}
	}
	return ok
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
		w.done <- webdavDo(client, req, cfg.URL)
		pw.Close()
	}()
	// Fail fast on an early rejection from the server.
	select {
	case err := <-w.done:
		return nil, err
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
		return fmt.Errorf("webdav: PUT %s: %w", what, err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webdav: PUT %s: %s", what, resp.Status)
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
	if w.spool != nil {
		spoolPath := w.spool.Name()
		spoolSize := w.size
		w.spool.Close()
		w.spool = nil
		if !abandon {
			f, openErr := os.Open(spoolPath)
			if openErr != nil {
				err = fmt.Errorf("webdav: reopen spool file: %w", openErr)
			} else {
				req, reqErr := http.NewRequest(http.MethodPut, w.cfg.URL, f)
				if reqErr != nil {
					f.Close()
					err = reqErr
				} else {
					req.ContentLength = spoolSize
					req.SetBasicAuth(w.cfg.User, w.cfg.Password)
					req.Header.Set("Content-Type", "application/octet-stream")
					err = webdavDo(w.client, req, w.cfg.URL)
					f.Close()
				}
			}
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
		case <-time.After(3 * time.Second):
			err = fmt.Errorf("webdav: transfer did not finish")
		}
	}

	if (abandon || err != nil) && w.cfg.URL != "" {
		// Remove the partial remote file (best effort).
		if dreq, derr := http.NewRequest(http.MethodDelete, w.cfg.URL, nil); derr == nil {
			dreq.SetBasicAuth(w.cfg.User, w.cfg.Password)
			if dresp, derr := w.client.Do(dreq); derr == nil {
				io.Copy(io.Discard, io.LimitReader(dresp.Body, 4096))
				dresp.Body.Close()
			}
		}
	}
	return err
}
