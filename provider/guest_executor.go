package provider

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type guestExecutor struct{}

func (p *CocoonProvider) guestExecutor() guestExecutor {
	return guestExecutor{}
}

func (guestExecutor) writeFile(ctx context.Context, vm *CocoonVM, password, path string, data []byte, mode int) error {
	dir := path[:strings.LastIndex(path, "/")]
	cmd := exec.CommandContext(ctx, "sshpass", "-p", password, //nolint:gosec // SSH args from pod spec
		"ssh", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR", "-o", "ConnectTimeout=5",
		fmt.Sprintf("root@%s", vm.ip),
		fmt.Sprintf("mkdir -p %s && cat > %s && chmod %04o %s", dir, path, mode, path))
	cmd.Stdin = strings.NewReader(string(data))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sshWriteFile %s: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (guestExecutor) execSimple(ctx context.Context, vm *CocoonVM, password, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sshpass", "-p", password, //nolint:gosec // SSH args from pod spec
		"ssh", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR", "-o", "ConnectTimeout=5",
		fmt.Sprintf("root@%s", vm.ip), command)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (guestExecutor) waitForSSH(ctx context.Context, vm *CocoonVM, password string, timeout time.Duration) error {
	if vm == nil || vm.ip == "" {
		return fmt.Errorf("vm has no IP")
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lastErr = sshReadyProbe(ctx, vm, password)
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ssh not ready after %s: %w", timeout, lastErr)
		}
		time.Sleep(sshReadyPollInterval)
	}
}
