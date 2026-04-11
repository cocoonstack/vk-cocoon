// Package guest is the host-side bridge into a cocoon VM. It
// covers the kubectl-style operations virtual-kubelet exposes (logs,
// exec, attach, port-forward) by tunneling them over SSH for Linux
// guests and a thin RDP help-text shim for Windows guests.
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

// defaultDialTimeout bounds how long a single connect attempt waits
// before giving up on an unreachable guest. Kept small so a dead
// VM does not wedge a `kubectl exec` for minutes.
const defaultDialTimeout = 10 * time.Second

// Executor runs commands inside the guest VM. It is satisfied by
// SSHExecutor for Linux guests and RDPExecutor for Windows guests
// (the latter only carries help text — Windows lacks a parallel for
// kubectl exec / logs).
type Executor interface {
	Run(ctx context.Context, host string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// SSHExecutor dials the guest over TCP and runs commands via a
// golang.org/x/crypto/ssh client. The implementation used to shell
// out to `sshpass`; the native client removes the subprocess and
// gets rid of the sshpass runtime dependency on host images.
//
// Host-key verification is disabled on purpose: cocoon VMs are
// cloned on every pod CREATE, so host keys rotate every pod
// lifetime. A TOFU cache would grow without bound and would trip on
// the first restore. The SSH is already scoped to a private
// cocoon-internal bridge network, not a routed path.
type SSHExecutor struct {
	User     string
	Password string
	Port     int
	// DialTimeout bounds a single TCP+SSH handshake. Zero falls
	// back to defaultDialTimeout so construction stays one-line.
	DialTimeout time.Duration
}

// NewSSHExecutor returns an SSHExecutor with the supplied
// credentials. port=0 resolves to 22.
func NewSSHExecutor(user, password string, port int) *SSHExecutor {
	if port == 0 {
		port = 22
	}
	return &SSHExecutor{User: user, Password: password, Port: port, DialTimeout: defaultDialTimeout}
}

// Run opens a fresh SSH connection to host, runs cmd, and streams
// stdin / stdout / stderr through the session. ctx cancellation
// closes the session so a stuck remote command interrupts cleanly.
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

	// Bridge ctx cancellation onto the session: when the caller
	// cancels we close the session, which interrupts the remote
	// command and unblocks session.Run below.
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

// FetchJournal runs `journalctl --no-pager -n <tailLines>` on the
// guest and returns its stdout. Used by GetContainerLogs for Linux
// guests.
func (e *SSHExecutor) FetchJournal(ctx context.Context, host string, tailLines int) ([]byte, error) {
	var out, errBuf bytes.Buffer
	cmd := []string{"journalctl", "--no-pager", "-n", strconv.Itoa(tailLines)}
	if err := e.Run(ctx, host, cmd, nil, &out, &errBuf); err != nil {
		return nil, fmt.Errorf("%w (stderr: %s)", err, errBuf.String())
	}
	return out.Bytes(), nil
}

// connect opens a fresh SSH client connection to host, honoring
// ctx for the TCP dial timeout.
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

// joinShell joins argv into a single POSIX-sh-safe command string
// suitable for ssh.Session.Run. x/crypto/ssh only exposes Run(string)
// so any multi-arg command has to be serialized here; posixQuote
// handles spaces, metacharacters, and embedded single quotes.
func joinShell(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = posixQuote(a)
	}
	return strings.Join(parts, " ")
}

// posixQuote wraps s in POSIX single quotes, escaping any embedded
// single quote as '\”. Empty and metacharacter-free strings get
// passed through unchanged to keep the wire format readable in
// session debug logs.
func posixQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !needsShellQuote(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// needsShellQuote reports whether s contains any character the
// POSIX shell would interpret, so posixQuote can skip the quoting
// overhead on simple binary names and numeric arguments.
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
