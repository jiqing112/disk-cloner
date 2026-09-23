package storage

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Minimal FTP client (RFC 959 + EPSV) — just enough for a streaming STOR.
// Hand-rolled because the available FTP modules are ancient and the command
// set used here is tiny: USER PASS TYPE EPSV PASV MKD STOR DELE QUIT.

type ftpClient struct {
	host string
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	// mu serializes all use of the control channel (command write + reply
	// read). The stor goroutine holds it while awaiting the final reply;
	// teardown paths acquire it (bounded) before sending DELE/QUIT so the
	// two sides never interleave reads on the same bufio.Reader.
	mu sync.Mutex
}

func ftpDial(host string, port int, timeout time.Duration) (*ftpClient, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return nil, err
	}
	c := &ftpClient{host: host, conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn)}
	code, _, err := c.readRespLocked()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ftp: no greeting: %w", err)
	}
	if code >= 400 {
		conn.Close()
		return nil, fmt.Errorf("ftp: server refused connection (%d)", code)
	}
	return c, nil
}

// Control-channel round-trip bounds. The TCP dial timeout only covers
// connection establishment; without these, a server that accepts TCP but
// never answers the protocol (hung ftpd, NAT black hole) blocks Open
// forever. Generous values: these cover greeting/login/EPSV/STOR-acks on
// slow NAS hardware, never the data transfer itself (separate connection).
const (
	ftpControlReadTimeout  = 60 * time.Second
	ftpControlWriteTimeout = 30 * time.Second
)

// readRespLocked reads one (possibly multi-line) FTP reply, e.g.
//
//	230-Go ahead
//	230 logged in
//
// The caller must hold c.mu.
func (c *ftpClient) readRespLocked() (int, string, error) {
	return c.readRespDeadlineLocked(ftpControlReadTimeout)
}

// readRespDeadlineLocked is readRespLocked with a caller-chosen reply
// deadline (teardown uses a tighter bound than normal operation).
func (c *ftpClient) readRespDeadlineLocked(d time.Duration) (int, string, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		return 0, "", err
	}
	var lines []string
	code := 0
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return 0, "", err
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, line)
		if len(line) >= 4 && line[3] == '-' {
			continue // continuation line
		}
		if len(line) >= 3 {
			if n, convErr := strconv.Atoi(line[:3]); convErr == nil {
				code = n
			}
		}
		break
	}
	if code == 0 {
		return 0, "", fmt.Errorf("ftp: malformed reply %q", strings.Join(lines, " | "))
	}
	return code, strings.Join(lines, "\n"), nil
}

// sendLocked writes one command line. The caller must hold c.mu.
func (c *ftpClient) sendLocked(command string) error {
	if hasCtl(command) {
		return fmt.Errorf("ftp: command contains control characters")
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(ftpControlWriteTimeout)); err != nil {
		return err
	}
	fmt.Fprint(c.w, command+"\r\n")
	return wFlush(c.w)
}

func (c *ftpClient) cmd(format string, args ...interface{}) (int, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.sendLocked(fmt.Sprintf(format, args...)); err != nil {
		return 0, "", err
	}
	return c.readRespLocked()
}

func wFlush(w *bufio.Writer) error { return w.Flush() }

// quitLocked sends QUIT and closes the connection. The caller must hold
// c.mu; the reply drain is bounded so a mute server cannot block teardown.
func (c *ftpClient) quitLocked() {
	c.sendLocked("QUIT")
	c.readRespDeadlineLocked(3 * time.Second)
	c.conn.Close()
}

func (c *ftpClient) quit() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.quitLocked()
}

// tryLock acquires the control-channel lock, giving up after d. Returns
// false when the stor goroutine still holds it (blocked reading a reply).
func (c *ftpClient) tryLock(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if c.mu.TryLock() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// teardown deletes the partial file (when asked) and closes the control
// connection. It runs only when the control channel is idle; when the stor
// goroutine still holds it, the connection is dropped instead so its
// blocked read errors out rather than stealing teardown's replies.
func (c *ftpClient) teardown(path string, delete bool) {
	if !c.tryLock(3 * time.Second) {
		c.conn.Close()
		return
	}
	defer c.mu.Unlock()
	if delete {
		c.sendLocked("DELE " + path)
	}
	c.quitLocked()
}

// login performs USER/PASS and fails on 5xx replies.
func (c *ftpClient) login(user, pass string) error {
	if user == "" {
		user = "anonymous"
	}
	code, msg, err := c.cmd("USER %s", user)
	if err != nil {
		return err
	}
	if code == 230 {
		return nil // already logged in
	}
	if code != 331 && code != 332 {
		return fmt.Errorf("ftp: USER %s: %d %s", user, code, msg)
	}
	code, msg, err = c.cmd("PASS %s", pass)
	if err != nil {
		return err
	}
	if code != 230 && code != 202 {
		return fmt.Errorf("ftp: login failed: %d %s", code, msg)
	}
	return nil
}

// openData opens the data connection via EPSV (fallback PASV). It connects
// to the CONTROL host with the negotiated port so NAT'd servers keep working.
func (c *ftpClient) openData() (net.Conn, error) {
	port := 0
	if code, msg, err := c.cmd("EPSV"); err == nil && code == 229 {
		port = ftpParseEPSV(msg)
	}
	if port == 0 {
		code, msg, err := c.cmd("PASV")
		if err != nil {
			return nil, err
		}
		if code != 227 {
			return nil, fmt.Errorf("ftp: PASV: %d %s", code, msg)
		}
		port = ftpParsePASV(msg)
	}
	if port <= 0 {
		return nil, fmt.Errorf("ftp: cannot parse passive-mode reply")
	}
	d, err := net.DialTimeout("tcp", net.JoinHostPort(c.host, strconv.Itoa(port)), 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("ftp: data connection: %w", err)
	}
	return d, nil
}

func ftpParseEPSV(msg string) int {
	open := strings.LastIndex(msg, "(")
	if open < 0 {
		return 0
	}
	for _, f := range strings.Split(strings.Trim(msg[open:], "()"), "|") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func ftpParsePASV(msg string) int {
	open := strings.LastIndex(msg, "(")
	if open < 0 {
		return 0
	}
	rest := msg[open+1:]
	// Replies often carry text after the closing paren —
	// "227 Entering passive mode (127,0,0,1,195,80)." (pyftpdlib, vsftpd).
	// Take exactly the parenthesized body; parsing failures are errors
	// (port 0), never silently-zeroed fields — a zeroed low byte dialed a
	// port 256 below the negotiated one.
	closing := strings.Index(rest, ")")
	if closing < 0 {
		return 0
	}
	f := strings.Split(rest[:closing], ",")
	if len(f) != 6 {
		return 0
	}
	p1, err1 := strconv.Atoi(strings.TrimSpace(f[4]))
	p2, err2 := strconv.Atoi(strings.TrimSpace(f[5]))
	if err1 != nil || err2 != nil || p1 < 0 || p2 < 0 {
		return 0
	}
	return p1*256 + p2
}

// stor streams r to path via STOR. r is read only after the server accepted
// the command, so a rejected path or permission fails before data flows.
// The accepted channel is closed as soon as the server answers 1xx, letting
// the caller proceed without waiting for the (much later) final reply.
func (c *ftpClient) stor(path string, r io.Reader, accepted chan<- struct{}) error {
	data, err := c.openData()
	if err != nil {
		return err
	}
	code, msg, err := c.cmd("STOR %s", path)
	if err != nil {
		data.Close()
		return err
	}
	if code/100 != 1 {
		data.Close()
		return fmt.Errorf("ftp: STOR %s: %d %s", path, code, msg)
	}
	close(accepted)
	_, copyErr := io.Copy(data, r)
	data.Close()
	// Hold the control-channel lock while reading the final reply so the
	// teardown path in finish() cannot interleave a DELE/SIZE on the same
	// bufio.Reader.
	c.mu.Lock()
	code2, msg2, respErr := c.readRespLocked()
	c.mu.Unlock()
	if copyErr != nil {
		return copyErr
	}
	if respErr != nil {
		return respErr
	}
	if code2/100 != 2 {
		return fmt.Errorf("ftp: STOR %s: %d %s", path, code2, msg2)
	}
	return nil
}

// mkdirAll best-effort creates parent directories (MKD, ignoring failures
// such as "already exists").
func (c *ftpClient) mkdirAll(p string) {
	abs := strings.HasPrefix(p, "/")
	p = strings.Trim(p, "/")
	if p == "" {
		return
	}
	segs := strings.Split(p, "/")
	prefix := ""
	if abs {
		prefix = "/"
	}
	for _, s := range segs[:len(segs)-1] {
		if s == "" {
			continue
		}
		if prefix != "/" {
			prefix += "/"
		}
		prefix += s
		c.cmd("MKD %s", prefix)
	}
}

func openFTP(cfg Config) (Writer, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("ftp: host is required")
	}
	if cfg.Port == 0 {
		cfg.Port = 21
	}
	if cfg.Path == "" {
		return nil, fmt.Errorf("ftp: destination path is required")
	}

	c, err := ftpDial(cfg.Host, cfg.Port, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("ftp: dial: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			c.quit()
		}
	}()

	if err := c.login(cfg.User, cfg.Password); err != nil {
		return nil, err
	}
	if code, msg, err := c.cmd("TYPE I"); err != nil || code >= 400 {
		return nil, fmt.Errorf("ftp: TYPE I: %d %s %v", code, msg, err)
	}
	c.mkdirAll(cfg.Path)

	pr, pw := io.Pipe()
	w := &ftpWriter{c: c, pw: pw, done: make(chan error, 1), accepted: make(chan struct{}), path: cfg.Path}
	go func() {
		err := c.stor(cfg.Path, pr, w.accepted)
		w.done <- err
		pw.Close() // unblock a writer parked in Write if the transfer dies
	}()
	// Fail fast on STOR rejection (permissions, bad path): the server
	// answers before any data is read from the pipe. Once it answers 1xx,
	// proceed immediately — the final 226 only arrives after EOF.
	select {
	case err := <-w.done:
		return nil, fmt.Errorf("ftp: STOR %s: %w", cfg.Path, err)
	case <-w.accepted:
	}
	ok = true
	return w, nil
}

type ftpWriter struct {
	c    *ftpClient
	pw   *io.PipeWriter
	done chan error
	// accepted is closed once the server replied 1xx to STOR.
	accepted chan struct{}
	path     string

	written atomic.Int64 // bytes handed to Write; compared against SIZE on finalize

	mu       sync.Mutex
	finished bool
}

func (w *ftpWriter) Write(p []byte) (int, error) {
	n, err := w.pw.Write(p)
	w.written.Add(int64(n))
	return n, err
}

// finalizeWait bounds how long Close waits for the server's final 226 after
// EOF. Large images can take well over a few seconds to flush to the
// server's disk, so this is deliberately generous.
const finalizeWait = 60 * time.Second

// Close finalizes the upload (server writes the file once it sees EOF). A
// failed/stalled transfer deletes the partial remote file — unless the
// outcome is unknown (final reply never arrived but the server may still
// have completed the file), in which case the file is kept and the error
// tells the user how to check, rather than risking deleting a complete
// backup.
func (w *ftpWriter) Close() error {
	return w.finish(false)
}

// Abort discards the upload and deletes the partial remote file.
func (w *ftpWriter) Abort() error {
	w.finish(true)
	return nil
}

// remoteSizeLocked queries the uploaded file's size via SIZE (TYPE I
// active). The caller must hold the control-channel lock.
func (c *ftpClient) remoteSizeLocked(path string) (int64, bool) {
	if err := c.sendLocked("SIZE " + path); err != nil {
		return 0, false
	}
	code, msg, err := c.readRespLocked()
	if err != nil || code != 213 {
		return 0, false
	}
	// readResp returns the reply WITH its numeric prefix ("213 4096") —
	// strip it or ParseInt always failed and this verification never
	// matched anything.
	n, err := strconv.ParseInt(strings.TrimSpace(ftpStripCode(msg)), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// ftpStripCode removes the leading 3-digit reply code (and separator) from
// a single-line FTP reply: "213 4096" → "4096". Anything that doesn't start
// with digits followed by a separator is returned unchanged.
func ftpStripCode(msg string) string {
	s := strings.TrimSpace(msg)
	if len(s) >= 4 {
		if _, err := strconv.Atoi(s[:3]); err == nil && (s[3] == ' ' || s[3] == '-') {
			return strings.TrimSpace(s[4:])
		}
	}
	return s
}

func (w *ftpWriter) finish(abandon bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return nil
	}
	w.finished = true
	if abandon {
		w.pw.CloseWithError(io.ErrClosedPipe)
	} else {
		w.pw.Close() // EOF → server finalizes
	}
	var err error
	timedOut := false
	select {
	case err = <-w.done:
	case <-time.After(finalizeWait):
		timedOut = true
		err = fmt.Errorf("ftp: no final reply from server within %s", finalizeWait)
	}
	if abandon {
		// The stor goroutine has exited (done received) or is stuck holding
		// the control channel (timed out) — teardown sends DELE only when
		// the channel is idle, otherwise it drops the connection and the
		// goroutine's blocked read errors out.
		w.c.teardown(w.path, true)
		return nil
	}
	if err != nil {
		// Server kept working past EOF: verify via SIZE whether the file is
		// complete before declaring failure. Only delete when the server
		// itself reported a transfer error — never on an unknown outcome.
		if timedOut {
			if w.c.tryLock(3 * time.Second) {
				complete := false
				if n, ok := w.c.remoteSizeLocked(w.path); ok && n == w.written.Load() {
					complete = true
				}
				w.c.quitLocked()
				if complete {
					return nil
				}
			} else {
				w.c.conn.Close()
			}
			return fmt.Errorf("%w (file kept on server — verify its size with SIZE/ls before trusting it)", err)
		}
		// The server itself reported a transfer error — the partial file is
		// definitely partial, remove it.
		w.c.teardown(w.path, true)
		return err
	}
	w.c.quit()
	return err
}
