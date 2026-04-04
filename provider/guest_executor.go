package provider

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type guestExecutor struct{}

func (p *CocoonProvider) guestExecutor() guestExecutor {
	return guestExecutor{}
}

func (guestExecutor) command(ctx context.Context, vm *CocoonVM, password string, tty bool, remoteArgs ...string) *exec.Cmd {
	args := []string{"-p", password, "ssh"}
	if tty {
		args = append(args, "-tt")
	}
	args = slices.Concat(args, []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		fmt.Sprintf("root@%s", vm.ip),
	}, remoteArgs)
	return exec.CommandContext(ctx, "sshpass", args...) //nolint:gosec // SSH args from pod spec
}

func (guestExecutor) writeFile(ctx context.Context, vm *CocoonVM, password, path string, data []byte, mode int) error {
	dir := filepath.Dir(path)
	cmd := guestExecutor{}.command(ctx, vm, password, false,
		fmt.Sprintf("mkdir -p %s && cat > %s && chmod %04o %s",
			shellQuoteJoin([]string{dir}), shellQuoteJoin([]string{path}), mode, shellQuoteJoin([]string{path})))
	cmd.Stdin = strings.NewReader(string(data))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh write file %s: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

type batchFile struct {
	path string
	data []byte
	mode int
}

func (guestExecutor) writeFilesBatch(ctx context.Context, vm *CocoonVM, password string, files []batchFile) error {
	if len(files) == 0 {
		return nil
	}
	if len(files) == 1 {
		return guestExecutor{}.writeFile(ctx, vm, password, files[0].path, files[0].data, files[0].mode)
	}

	var script strings.Builder
	script.WriteString("set -e\n")
	for i, f := range files {
		dir := filepath.Dir(f.path)
		marker := fmt.Sprintf("__EOF_BATCH_%d__", i)
		fmt.Fprintf(&script, "mkdir -p '%s'\n", dir)
		fmt.Fprintf(&script, "cat > '%s' <<%s\n", f.path, marker)
		script.Write(f.data)
		if len(f.data) > 0 && f.data[len(f.data)-1] != '\n' {
			script.WriteByte('\n')
		}
		fmt.Fprintf(&script, "%s\n", marker)
		fmt.Fprintf(&script, "chmod %04o '%s'\n", f.mode, f.path)
	}

	cmd := guestExecutor{}.command(ctx, vm, password, false, "bash -s")
	cmd.Stdin = strings.NewReader(script.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh batch write %d files: %w (%s)", len(files), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (guestExecutor) execSimple(ctx context.Context, vm *CocoonVM, password, command string) (string, error) {
	cmd := guestExecutor{}.command(ctx, vm, password, false, command)
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
