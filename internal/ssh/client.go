package ssh

import (
	"fmt"
	"io"
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

	// Try all available SSH keys — let the SSH library attempt each
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
