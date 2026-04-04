package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

func (p *EpochPuller) cocoonExec(ctx context.Context, args ...string) (string, error) { //nolint:unparam // string return used by callers for error context
	cmd := p.buildCocoonCmd(ctx, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (p *EpochPuller) cocoonExecWithStdin(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	cmd := p.buildCocoonCmd(ctx, args...)
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (p *EpochPuller) buildCocoonCmd(ctx context.Context, args ...string) *exec.Cmd {
	var sudoArgs []string
	root := strings.TrimSpace(os.Getenv("COCOON_ROOT_DIR"))
	if root == "" {
		root = strings.TrimSpace(p.rootDir)
	}
	if root != "" {
		sudoArgs = append(sudoArgs, "env", "COCOON_ROOT_DIR="+root)
	}
	if windowsCH := strings.TrimSpace(os.Getenv("COCOON_WINDOWS_CH_BINARY")); windowsCH != "" {
		if len(sudoArgs) == 0 {
			sudoArgs = append(sudoArgs, "env")
		}
		sudoArgs = append(sudoArgs, "COCOON_WINDOWS_CH_BINARY="+windowsCH)
	}
	sudoArgs = append(sudoArgs, p.cocoonBin)
	sudoArgs = append(sudoArgs, args...)
	return exec.CommandContext(ctx, "sudo", sudoArgs...) //nolint:gosec
}

func (p *EpochPuller) pipeToImport(ctx context.Context, args []string, writeFn func(w io.Writer) error) error {
	pr, pw := io.Pipe()

	cmdDone := make(chan error, 1)
	go func() {
		out, err := p.cocoonExecWithStdin(ctx, pr, args...)
		if err != nil {
			cmdDone <- fmt.Errorf("cocoon %s: %s: %w", args[0], strings.TrimSpace(out), err)
		} else {
			cmdDone <- nil
		}
	}()

	writeErr := writeFn(pw)
	_ = pw.CloseWithError(writeErr)

	if importErr := <-cmdDone; importErr != nil {
		if writeErr != nil {
			return fmt.Errorf("stream: %w, import: %w", writeErr, importErr)
		}
		return importErr
	}
	return writeErr
}

func (p *EpochPuller) copyBlob(ctx context.Context, name, digest string, w io.Writer) error {
	url := p.blobURL(name, digest)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create blob request: %w", err)
	}
	p.setAuth(req)

	resp, err := p.client.Do(req) //nolint:gosec // epoch serverURL is configured by the trusted provider setup
	if err != nil {
		return fmt.Errorf("get blob %s: %w", shortHex(digest), err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get blob %s: %d %s", shortHex(digest), resp.StatusCode, readLimitedBody(resp.Body))
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("read blob %s: %w", shortHex(digest), err)
	}
	return nil
}

func (p *EpochPuller) putBlob(ctx context.Context, name, digest string, body io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.blobURL(name, digest), body)
	if err != nil {
		return fmt.Errorf("create put request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	p.setAuth(req)

	resp, err := p.client.Do(req) //nolint:gosec // epoch serverURL is configured by the trusted provider setup
	if err != nil {
		return fmt.Errorf("put blob: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("put blob %s: %d %s", shortHex(digest), resp.StatusCode, readLimitedBody(resp.Body))
	}
	return nil
}
