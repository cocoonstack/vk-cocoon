// Package ssh implements the guest.Executor interface for Linux VMs
// via golang.org/x/crypto/ssh.
package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cocoonstack/vk-cocoon/guest"
)

const (
	defaultDialTimeout = 10 * time.Second
	defaultPort        = 22
)

// compile-time interface check.
var (
	_ guest.Executor = (*Executor)(nil)
)

// Executor runs commands on guests via golang.org/x/crypto/ssh.
// Host-key verification is disabled because VMs rotate keys on every clone.
type Executor struct {
	User        string
	Password    string
	Port        int
	DialTimeout time.Duration
}

// NewExecutor returns an Executor; port=0 defaults to 22.
func NewExecutor(user, password string, port int) *Executor {
	if port == 0 {
		port = defaultPort
	}
	return &Executor{User: user, Password: password, Port: port, DialTimeout: defaultDialTimeout}
}

// Run opens an SSH connection, runs cmd, and streams I/O.
// ctx cancellation interrupts cleanly.
func (e *Executor) Run(ctx context.Context, host string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
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

	if err := session.Run(guest.JoinShell(cmd)); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("ssh exec %s on %s: %w", cmd[0], host, ctxErr)
		}
		return fmt.Errorf("ssh exec %s on %s: %w", cmd[0], host, err)
	}
	return nil
}

func (e *Executor) connect(ctx context.Context, host string) (*ssh.Client, error) {
	timeout := e.DialTimeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	cfg := &ssh.ClientConfig{
		User:            e.User,
		Auth:            []ssh.AuthMethod{ssh.Password(e.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // cocoon VMs rotate host keys every clone
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
