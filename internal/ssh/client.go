package ssh

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type Client struct {
	conn   *ssh.Client
	Config Config
}

type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	Timeout  int
}

// ProbeResult contains the SSH server banner received during pre-connection probe.
type ProbeResult struct {
	Banner string
}

// ProbeSSH opens a raw TCP connection to read the SSH server banner
// before attempting a full SSH handshake. This helps diagnose "handshake
// failed: EOF" errors by showing whether the server actually speaks SSH.
func ProbeSSH(cfg Config) (*ProbeResult, error) {
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("tcp connect: %w", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("no banner received (server closed connection without sending SSH version string): %w", err)
	}

	banner := strings.TrimSpace(string(buf[:n]))
	return &ProbeResult{Banner: banner}, nil
}

func Connect(cfg Config) (*Client, error) {
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	if cfg.User == "" {
		cfg.User = "root"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30
	}

	authMethods := []ssh.AuthMethod{}

	// Password auth (tried first if provided)
	if cfg.Password != "" {
		authMethods = append(authMethods, ssh.Password(cfg.Password))
	}

	// Try all available SSH keys
	keyPaths := []string{"~/.ssh/id_rsa", "~/.ssh/id_ed25519", "~/.ssh/id_ecdsa"}
	for _, kp := range keyPaths {
		expanded := expandPath(kp)
		key, err := os.ReadFile(expanded)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			continue
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication method available (provide password or SSH key)")
	}

	sshCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Duration(cfg.Timeout) * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	conn, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		// Enhance the error message for common Windows-specific issues
		errMsg := err.Error()
		if strings.Contains(errMsg, "handshake failed: EOF") {
			return nil, fmt.Errorf("ssh handshake: server closed connection before completing SSH protocol\n"+
				"  Possible causes:\n"+
				"  - Windows firewall or antivirus is intercepting SSH traffic\n"+
				"  - Corporate proxy/VPN is blocking SSH protocol on port 22\n"+
				"  - Server has IP-based access restrictions (check /etc/hosts.allow)\n"+
				"  - Try a different network (mobile hotspot) to rule out firewall\n"+
				"  Original error: %s", errMsg)
		}
		return nil, fmt.Errorf("ssh dial: %w", err)
	}

	return &Client{conn: conn, Config: cfg}, nil
}

// Execute starts a command on the remote server and returns a Session
// with Stdout and Stderr readers. The caller must call session.Close().
func (c *Client) Execute(cmd string) (*Session, error) {
	session, err := c.conn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return nil, err
	}

	stderr, err := session.StderrPipe()
	if err != nil {
		session.Close()
		return nil, err
	}

	if err := session.Start(cmd); err != nil {
		session.Close()
		return nil, err
	}

	return &Session{
		session: session,
		Stdout:  stdout,
		Stderr:  stderr,
	}, nil
}

// ExecuteStdin starts a command on the remote and returns a session
// with Stdin pipe for sending data to the remote command.
// The caller must call session.Close() when done.
func (c *Client) ExecuteStdin(cmd string) (*StdinSession, error) {
	session, err := c.conn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return nil, err
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return nil, err
	}

	stderr, err := session.StderrPipe()
	if err != nil {
		session.Close()
		return nil, err
	}

	if err := session.Start(cmd); err != nil {
		session.Close()
		return nil, err
	}

	return &StdinSession{
		Session: &Session{
			session: session,
			Stdout:  stdout,
			Stderr:  stderr,
		},
		Stdin: stdin,
	}, nil
}

// CombinedOutput runs a command and returns stdout+stderr combined.
func (c *Client) CombinedOutput(cmd string) (string, error) {
	session, err := c.conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("new session: %w", err)
	}
	defer session.Close()
	out, err := session.CombinedOutput(cmd)
	return string(out), err
}

func (c *Client) Close() {
	if c.conn != nil {
		c.conn.Close()
	}
}

type Session struct {
	session *ssh.Session
	Stdout  io.Reader
	Stderr  io.Reader
}

// StdinSession wraps an SSH session that accepts stdin data.
// Used for push/restore operations where local data is piped to the remote.
type StdinSession struct {
	*Session
	Stdin io.WriteCloser
}

func (s *StdinSession) Close() {
	if s.Stdin != nil {
		s.Stdin.Close()
	}
	if s.Session != nil {
		s.Session.Close()
	}
}

func (s *Session) Wait() error {
	return s.session.Wait()
}

func (s *Session) Close() {
	s.session.Close()
}

func (s *Session) Signal(sig ssh.Signal) error {
	return s.session.Signal(sig)
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return strings.Replace(path, "~", home, 1)
		}
	}
	return path
}
