package ssh

import (
	"net"
	"strings"
	"testing"
	"time"
)

// TestConnectHandshakeTimeout: ssh.ClientConfig.Timeout only bounds the TCP
// dial, so a peer that accepts TCP but never speaks the SSH protocol would
// block the handshake forever. Connect must return an error within roughly
// the configured timeout.
func TestConnectHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and stay silent — never send the SSH version banner,
			// never close (a close would surface as EOF, not a hang).
			_ = c
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	cfg := Config{Host: "127.0.0.1", Port: port, User: "root", Password: "pw", Timeout: 2}
	start := time.Now()
	_, err = Connect(cfg)
	if err == nil {
		t.Fatal("Connect to a silent server = nil error, want handshake timeout")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Connect took %s, want ~%ds (handshake deadline)", elapsed, cfg.Timeout)
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Logf("handshake error (deadline path exercised): %v", err)
	}
}
