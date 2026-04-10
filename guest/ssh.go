// Package guest is the host-side bridge into a cocoon VM. It
// covers the kubectl-style operations virtual-kubelet exposes (logs,
// exec, attach, port-forward) by tunneling them over SSH for Linux
// guests and a thin RDP help-text shim for Windows guests.
package guest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
)

// Executor runs commands inside the guest VM. It is satisfied by
// SSHExecutor for Linux guests and RDPExecutor for Windows guests
// (the latter only carries help text — Windows lacks a parallel for
// kubectl exec / logs).
type Executor interface {
	Run(ctx context.Context, host string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// SSHExecutor runs `sshpass -p <password> ssh <user>@<host> ...`.
// Production hosts ship sshpass; tests use a fake.
type SSHExecutor struct {
	User     string
	Password string
	Port     int
	// Binary is the SSH client binary path; "" means /usr/bin/ssh.
	Binary string
}

// NewSSHExecutor returns an SSHExecutor with the supplied
// credentials. port=0 resolves to 22.
func NewSSHExecutor(user, password string, port int) *SSHExecutor {
	if port == 0 {
		port = 22
	}
	return &SSHExecutor{User: user, Password: password, Port: port}
}

// Run executes the command on host and forwards stdin / stdout /
// stderr through the SSH session.
func (e *SSHExecutor) Run(ctx context.Context, host string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if host == "" {
		return fmt.Errorf("ssh: host is required")
	}
	if len(cmd) == 0 {
		return fmt.Errorf("ssh: command is required")
	}

	args := []string{
		"-p", e.Password,
		"ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-p", fmt.Sprintf("%d", e.Port),
		fmt.Sprintf("%s@%s", e.User, host),
	}
	args = append(args, cmd...)

	bin := "sshpass"
	c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // host comes from operator-managed annotations
	c.Stdin = stdin
	c.Stdout = stdout
	c.Stderr = stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("ssh exec %s on %s: %w", cmd[0], host, err)
	}
	return nil
}

// FetchJournal runs `journalctl --no-pager -n <tailLines>` on the
// guest and returns its stdout. Used by GetContainerLogs for Linux
// guests.
func (e *SSHExecutor) FetchJournal(ctx context.Context, host string, tailLines int) ([]byte, error) {
	var out, errBuf bytes.Buffer
	cmd := []string{"journalctl", "--no-pager", "-n", fmt.Sprintf("%d", tailLines)}
	if err := e.Run(ctx, host, cmd, nil, &out, &errBuf); err != nil {
		return nil, fmt.Errorf("%w (stderr: %s)", err, errBuf.String())
	}
	return out.Bytes(), nil
}
