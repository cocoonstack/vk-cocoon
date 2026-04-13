// Package guest bridges kubectl operations into cocoon VMs via SSH (Linux) or RDP help-text (Windows).
package guest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// defaultDialTimeout bounds a single connect attempt.
	defaultDialTimeout = 10 * time.Second
)

// Executor runs commands inside a guest VM.
type Executor interface {
	Run(ctx context.Context, host string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// SSHExecutor runs commands on guests via golang.org/x/crypto/ssh.
// Host-key verification is disabled because VMs rotate keys on every clone.
type SSHExecutor struct {
	User        string
	Password    string
	Port        int
	DialTimeout time.Duration
}

// NewSSHExecutor returns an SSHExecutor; port=0 defaults to 22.
func NewSSHExecutor(user, password string, port int) *SSHExecutor {
	if port == 0 {
		port = 22
	}
	return &SSHExecutor{User: user, Password: password, Port: port, DialTimeout: defaultDialTimeout}
}

// Run opens an SSH connection, runs cmd, and streams I/O. ctx cancellation interrupts cleanly.
func (e *SSHExecutor) Run(ctx context.Context, host string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if host == "" {
		return errors.New("ssh: host is required")
	}
	if len(cmd) == 0 {
		return errors.New("ssh: command is required")
	}

	cli, err := e.connect(ctx, host)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	session, err := cli.NewSession()
	if err != nil {
		return fmt.Errorf("open ssh session on %s: %w", host, err)
	}
	defer func() { _ = session.Close() }()

	session.Stdin = stdin
	session.Stdout = stdout
	session.Stderr = stderr

	// Bridge ctx cancellation onto the session.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Close()
		case <-done:
		}
	}()

	if err := session.Run(joinShell(cmd)); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("ssh exec %s on %s: %w", cmd[0], host, ctxErr)
		}
		return fmt.Errorf("ssh exec %s on %s: %w", cmd[0], host, err)
	}
	return nil
}

// FetchJournal runs journalctl on the guest and returns stdout.
func (e *SSHExecutor) FetchJournal(ctx context.Context, host string, tailLines int) ([]byte, error) {
	var out, errBuf bytes.Buffer
	cmd := []string{"journalctl", "--no-pager", "-n", strconv.Itoa(tailLines)}
	if err := e.Run(ctx, host, cmd, nil, &out, &errBuf); err != nil {
		return nil, fmt.Errorf("%w (stderr: %s)", err, errBuf.String())
	}
	return out.Bytes(), nil
}

// connect opens an SSH client connection to host.
func (e *SSHExecutor) connect(ctx context.Context, host string) (*ssh.Client, error) {
	timeout := e.DialTimeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	cfg := &ssh.ClientConfig{
		User:            e.User,
		Auth:            []ssh.AuthMethod{ssh.Password(e.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // cocoon VMs rotate host keys every clone; see SSHExecutor doc.
		Timeout:         timeout,
	}
	addr := net.JoinHostPort(host, strconv.Itoa(e.Port))
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// joinShell joins argv into a POSIX-sh-safe command string for ssh.Session.Run.
func joinShell(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = posixQuote(a)
	}
	return strings.Join(parts, " ")
}

// posixQuote wraps s in POSIX single quotes, passing simple strings through unchanged.
func posixQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !needsShellQuote(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// needsShellQuote reports whether s contains shell metacharacters.
func needsShellQuote(s string) bool {
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			continue
		case c == '_', c == '-', c == '.', c == '/', c == ':', c == '=', c == '@', c == '+', c == ',':
			continue
		default:
			return true
		}
	}
	return false
}
