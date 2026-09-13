package storage

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
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
}

func ftpDial(host string, port int, timeout time.Duration) (*ftpClient, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return nil, err
	}
	c := &ftpClient{host: host, conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn)}
	code, _, err := c.readResp()
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

// readResp reads one (possibly multi-line) FTP reply, e.g.
//
//	230-Go ahead
//	230 logged in
func (c *ftpClient) readResp() (int, string, error) {
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

func (c *ftpClient) cmd(format string, args ...interface{}) (int, string, error) {
	fmt.Fprintf(c.w, format+"\r\n", args...)
	if err := wFlush(c.w); err != nil {
		return 0, "", err
	}
	return c.readResp()
}

func wFlush(w *bufio.Writer) error { return w.Flush() }

func (c *ftpClient) quit() {
	c.cmd("QUIT")
	c.conn.Close()
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
	f := strings.Split(strings.Trim(msg[open:], "()"), ",")
	if len(f) != 6 {
		return 0
	}
	p1, _ := strconv.Atoi(strings.TrimSpace(f[4]))
	p2, _ := strconv.Atoi(strings.TrimSpace(f[5]))
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
	code2, msg2, respErr := c.readResp()
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

	mu       sync.Mutex
	finished bool
}

func (w *ftpWriter) Write(p []byte) (int, error) {
	return w.pw.Write(p)
}

// Close finalizes the upload (server writes the file once it sees EOF). On
// error the partial remote file is deleted before the control connection
// goes away.
func (w *ftpWriter) Close() error {
	return w.finish(false)
}

// Abort discards the upload and deletes the partial remote file.
func (w *ftpWriter) Abort() error {
	w.finish(true)
	return nil
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
	select {
	case err = <-w.done:
	case <-time.After(3 * time.Second):
		err = fmt.Errorf("ftp: transfer did not finish")
	}
	if abandon || err != nil {
		w.c.cmd("DELE %s", w.path) // remove partial, ignore errors
	}
	w.c.quit()
	return err
}
