package storage

import (
	"fmt"
	"path"

	sshclient "disk-cloner/internal/ssh"
	"github.com/pkg/sftp"
)

// openSFTP connects to the storage host over SSH, starts the sftp subsystem
// and opens the destination file for writing (create + truncate). It fails
// fast so bad hosts, credentials or paths surface before any disk data
// flows.
func openSFTP(cfg Config) (Writer, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("sftp: host is required")
	}
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	if cfg.Path == "" {
		return nil, fmt.Errorf("sftp: destination path is required")
	}

	c, err := sshclient.Connect(sshclient.Config{
		Host: cfg.Host, Port: cfg.Port, User: cfg.User, Password: cfg.Password, Timeout: 30,
	})
	if err != nil {
		return nil, fmt.Errorf("sftp: ssh connect: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()

	cli, err := sftp.NewClient(c.Raw())
	if err != nil {
		return nil, fmt.Errorf("sftp: start subsystem (server may not support SFTP): %w", err)
	}

	// Create missing parent directories (best effort — Create fails below
	// if this couldn't be done).
	if dir := path.Dir(cfg.Path); dir != "" && dir != "." && dir != "/" {
		cli.MkdirAll(dir)
	}

	f, err := cli.Create(cfg.Path)
	if err != nil {
		cli.Close()
		return nil, fmt.Errorf("sftp: create %s: %w", cfg.Path, err)
	}
	ok = true
	return &sftpWriter{ssh: c, client: cli, file: f, path: cfg.Path, done: false}, nil
}

type sftpWriter struct {
	ssh    *sshclient.Client
	client *sftp.Client
	file   *sftp.File
	path   string
	done   bool // Close/Abort already ran (they both terminate the upload)
}

func (w *sftpWriter) Write(p []byte) (int, error) {
	return w.file.Write(p)
}

// Close finalizes the upload. On error the partial remote file is removed
// (while the connection is still open).
func (w *sftpWriter) Close() error {
	if w.done {
		return nil
	}
	w.done = true
	err := w.file.Close()
	if err != nil {
		w.removePartial()
	}
	if w.client != nil {
		w.client.Close()
		w.client = nil
	}
	w.ssh.Close()
	if err != nil {
		return fmt.Errorf("sftp: close %s: %w", w.path, err)
	}
	return nil
}

// Abort removes the partial remote file (best effort).
func (w *sftpWriter) Abort() error {
	if w.done {
		return nil
	}
	w.done = true
	err := w.removePartial()
	if w.client != nil {
		w.client.Close()
		w.client = nil
	}
	w.ssh.Close()
	return err
}

func (w *sftpWriter) removePartial() error {
	if w.client == nil {
		return nil
	}
	if err := w.client.Remove(w.path); err != nil {
		return fmt.Errorf("sftp: remove partial %s: %w", w.path, err)
	}
	return nil
}
