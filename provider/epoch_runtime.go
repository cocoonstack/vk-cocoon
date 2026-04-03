package provider

import (
	"bytes"
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

// cocoonExecWithStdin runs a cocoon subprocess with data piped to stdin.
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

// pipeToImport streams data into a cocoon import subprocess via pipe.
// writeFn writes the data to w; the cocoon command reads from the pipe.
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
	if p.authToken() != "" {
		return p.curlCopy(ctx, url, w)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	p.setAuth(req)

	resp, err := p.client.Do(req) //nolint:gosec // epoch serverURL is configured by the trusted provider setup
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get blob %s: %d %s", digest[:12], resp.StatusCode, readLimitedBody(resp.Body))
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return err
	}
	return nil
}

func (p *EpochPuller) curlCopy(ctx context.Context, url string, w io.Writer) error {
	args := []string{"-fsSL", "--retry", "3"}
	if token := p.authToken(); token != "" {
		args = append(args, "-H", "Authorization: Bearer "+token)
	}
	args = append(args, url)

	cmd := exec.CommandContext(ctx, "curl", args...) //nolint:gosec
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("curl GET %s: stdout pipe: %w", url, err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("curl GET %s: %w", url, err)
	}

	if _, err := io.Copy(w, stdout); err != nil {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		return fmt.Errorf("curl GET %s: %w", url, err)
	}
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("curl GET %s: %s: %w", url, msg, err)
		}
		return fmt.Errorf("curl GET %s: %w", url, err)
	}
	return nil
}

// putBlob uploads a blob to the registry via PUT /v2/{name}/blobs/sha256:{digest}.
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
		return fmt.Errorf("put blob %s: %d %s", digest[:12], resp.StatusCode, readLimitedBody(resp.Body))
	}
	return nil
}
