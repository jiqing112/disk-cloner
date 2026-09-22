package storage

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFTPServer speaks enough of the FTP protocol on a local TCP listener
// to exercise login, TYPE, EPSV, STOR and DELE. When failSTOR is set the
// server rejects STOR with 550 (permission denied).
type fakeFTPServer struct {
	t        *testing.T
	ln       net.Listener
	dataCh   chan []byte
	mu       sync.Mutex
	loggedIn bool
	failSTOR bool
	sawDELE  bool
	storPath string
	passOK   bool
}

func startFakeFTP(t *testing.T, failSTOR bool) *fakeFTPServer {
	s := &fakeFTPServer{t: t, dataCh: make(chan []byte, 1), failSTOR: failSTOR}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	go s.serve()
	return s
}

func (s *fakeFTPServer) addr() string { return s.ln.Addr().String() }

func (s *fakeFTPServer) serve() {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	fmt.Fprint(conn, "220 fake FTP ready\r\n")

	var dataLn net.Listener
	defer func() {
		if dataLn != nil {
			dataLn.Close()
		}
	}()

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		s.mu.Lock()
		switch {
		case cmd == "QUIT":
			fmt.Fprint(conn, "221 bye\r\n")
			s.mu.Unlock()
			return
		case strings.HasPrefix(cmd, "USER"):
			fmt.Fprint(conn, "331 need password\r\n")
		case strings.HasPrefix(cmd, "PASS"):
			if cmd == "PASS good" {
				s.loggedIn = true
				fmt.Fprint(conn, "230 logged in\r\n")
			} else {
				fmt.Fprint(conn, "530 Login incorrect\r\n")
			}
		case strings.HasPrefix(cmd, "TYPE"):
			fmt.Fprint(conn, "200 Type set\r\n")
		case strings.HasPrefix(cmd, "MKD"):
			fmt.Fprint(conn, "257 created\r\n")
		case cmd == "EPSV":
			if dataLn != nil {
				dataLn.Close()
			}
			dataLn, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				s.mu.Unlock()
				return
			}
			fmt.Fprintf(conn, "229 Entering Extended Passive Mode (|||%d|)\r\n", dataLn.Addr().(*net.TCPAddr).Port)
		case strings.HasPrefix(cmd, "STOR"):
			if !s.loggedIn {
				fmt.Fprint(conn, "530 Please login\r\n")
			} else if s.failSTOR {
				fmt.Fprint(conn, "550 Permission denied\r\n")
			} else {
				s.storPath = strings.TrimPrefix(cmd, "STOR ")
				fmt.Fprint(conn, "150 Opening data connection\r\n")
				dconn, derr := dataLn.Accept()
				if derr != nil {
					s.mu.Unlock()
					return
				}
				buf, _ := io.ReadAll(dconn)
				dconn.Close()
				s.dataCh <- buf
				fmt.Fprint(conn, "226 Transfer complete\r\n")
			}
		case strings.HasPrefix(cmd, "DELE"):
			s.sawDELE = true
			fmt.Fprint(conn, "250 deleted\r\n")
		default:
			fmt.Fprint(conn, "502 not implemented\r\n")
		}
		s.mu.Unlock()
	}
}

func ftpCfg(addr string, path string, pass string) Config {
	host, port := splitHostPort(addr)
	return Config{Kind: KindFTP, Host: host, Port: port, User: "u", Password: pass, Path: path}
}

func splitHostPort(addr string) (string, int) {
	i := strings.LastIndex(addr, ":")
	host := addr[:i]
	port := 0
	fmt.Sscanf(addr[i+1:], "%d", &port)
	return host, port
}

func TestFTPUploadEndToEnd(t *testing.T) {
	srv := startFakeFTP(t, false)
	defer srv.ln.Close()

	w, err := Open(ftpCfg(srv.addr(), "backup/disk.img.gz", "good"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	payload := bytes.Repeat([]byte("XYZ"), 100000) // 300KB, several pipe writes
	if n, err := w.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case got := <-srv.dataCh:
		if !bytes.Equal(got, payload) {
			t.Fatalf("data mismatch: got %d bytes, want %d", len(got), len(payload))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for uploaded data")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.storPath != "backup/disk.img.gz" {
		t.Errorf("STOR path = %q", srv.storPath)
	}
}

func TestFTPBadPasswordFailsFast(t *testing.T) {
	srv := startFakeFTP(t, false)
	defer srv.ln.Close()
	if _, err := Open(ftpCfg(srv.addr(), "x.img.gz", "wrong")); err == nil || !strings.Contains(err.Error(), "login failed") {
		t.Fatalf("Open = %v, want login failure", err)
	}
}

func TestFTPStorRejectedFailsFast(t *testing.T) {
	srv := startFakeFTP(t, true)
	defer srv.ln.Close()
	_, err := Open(ftpCfg(srv.addr(), "x.img.gz", "good"))
	if err == nil || !strings.Contains(err.Error(), "550") {
		t.Fatalf("Open = %v, want STOR rejection", err)
	}
}

func TestFTPAbortDeletesPartial(t *testing.T) {
	srv := startFakeFTP(t, false)
	defer srv.ln.Close()

	w, err := Open(ftpCfg(srv.addr(), "x.img.gz", "good"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	w.Write([]byte("partial-data"))
	w.Abort()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		dele := srv.sawDELE
		srv.mu.Unlock()
		if dele {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server never saw DELE after Abort")
}

// TestFTPParsePASV: replies often carry text after the closing paren —
// "227 Entering passive mode (127,0,0,1,195,80)." (pyftpdlib, vsftpd). The
// parser must take exactly the parenthesized body and treat parse failures
// as errors, never silently-zeroed port bytes (that dialed port-256-below
// the negotiated one).
func TestFTPParsePASV(t *testing.T) {
	cases := []struct {
		msg  string
		want int
	}{
		{"227 Entering passive mode (127,0,0,1,195,80).", 50000}, // trailing period
		{"227 Entering Passive Mode (127,0,0,1,195,80)", 50000},  // bare
		{"227 Entering passive mode (127,0,0,1,195,80) extra text", 50000},
		{"227 Entering passive mode (127,0,0,1,195,x).", 0},  // non-numeric port byte
		{"227 no parens here", 0},                            // malformed
		{"227 (1,2,3)", 0},                                   // too few fields
	}
	for _, c := range cases {
		if got := ftpParsePASV(c.msg); got != c.want {
			t.Errorf("ftpParsePASV(%q) = %d, want %d", c.msg, got, c.want)
		}
	}
}

// TestFTPStripCode: readResp returns replies WITH their numeric prefix —
// remoteSize must strip "213 " before parsing the size, or the SIZE-based
// completeness check never matched anything.
func TestFTPStripCode(t *testing.T) {
	cases := map[string]string{
		"213 4096":  "4096",
		"213-4096":  "4096",
		"4096":      "4096", // no code prefix — returned unchanged
		"213  8192": "8192",
	}
	for in, want := range cases {
		if got := ftpStripCode(in); got != want {
			t.Errorf("ftpStripCode(%q) = %q, want %q", in, got, want)
		}
	}
}
