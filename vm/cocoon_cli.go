package vm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// defaultCocoonBinary is the path used when COCOON_BIN is unset.
	defaultCocoonBinary = "/usr/local/bin/cocoon"
)

// Compile-time guarantee that CocoonCLI satisfies the Runtime
// interface vk-cocoon consumes.
var _ Runtime = (*CocoonCLI)(nil)

// CocoonCLI is the production Runtime that shells out to the local
// `cocoon` binary. The binary path is resolved once in
// NewCocoonCLI; tests use a fake.
type CocoonCLI struct {
	// binary is the resolved path to the cocoon executable.
	binary string
	// sudo runs the command via sudo when true. Required on most
	// production hosts because cocoon needs root.
	sudo bool
}

// NewCocoonCLI returns a CocoonCLI that runs the cocoon binary at
// the supplied path; an empty path resolves to defaultCocoonBinary.
// The binary path is captured here so the hot path does not pay a
// branch on every Clone / Run / Inspect call.
func NewCocoonCLI(binary string, sudo bool) *CocoonCLI {
	if binary == "" {
		binary = defaultCocoonBinary
	}
	return &CocoonCLI{binary: binary, sudo: sudo}
}

// command builds an *exec.Cmd that invokes cocoon with the supplied
// args, optionally wrapped in sudo.
func (c *CocoonCLI) command(ctx context.Context, args ...string) *exec.Cmd {
	if c.sudo {
		full := append([]string{c.binary}, args...)
		return exec.CommandContext(ctx, "sudo", full...) //nolint:gosec // path comes from operator config, not untrusted input
	}
	return exec.CommandContext(ctx, c.binary, args...) //nolint:gosec // see above
}

// Clone runs `cocoon vm clone --name <To> <From>`.
func (c *CocoonCLI) Clone(ctx context.Context, opts CloneOptions) (*VM, error) {
	args := []string{"vm", "clone"}
	if opts.To != "" {
		args = append(args, "--name", opts.To)
	}
	args = appendCreateArgs(args, opts.CPU, opts.Memory, opts.Network, opts.Storage, opts.NICs, opts.DNS)
	args = append(args, opts.From)
	if _, err := c.runJSON(ctx, args...); err != nil {
		return nil, fmt.Errorf("cocoon vm clone: %w", err)
	}
	return c.Inspect(ctx, opts.To)
}

// Run runs `cocoon vm run <image>`.
func (c *CocoonCLI) Run(ctx context.Context, opts RunOptions) (*VM, error) {
	if err := c.EnsureImage(ctx, opts.Image); err != nil {
		return nil, fmt.Errorf("ensure image %s: %w", opts.Image, err)
	}

	args := []string{"vm", "run"}
	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}
	args = appendCreateArgs(args, opts.CPU, opts.Memory, opts.Network, opts.Storage, opts.NICs, opts.DNS)
	if strings.EqualFold(opts.OS, "windows") {
		args = append(args, "--windows")
	}
	args = append(args, opts.Image)
	if _, err := c.runJSON(ctx, args...); err != nil {
		return nil, fmt.Errorf("cocoon vm run: %w", err)
	}
	return c.Inspect(ctx, opts.Name)
}

// EnsureImage makes sure the cocoon image store can resolve image before vm
// run. Local image names succeed via `cocoon image inspect`; remote OCI refs
// and cloud-image URLs fall back to `cocoon image pull`.
func (c *CocoonCLI) EnsureImage(ctx context.Context, image string) error {
	if image == "" {
		return nil
	}
	if err := c.command(ctx, "image", "inspect", image).Run(); err == nil {
		return nil
	}
	out, err := c.command(ctx, "image", "pull", image).CombinedOutput()
	if err != nil {
		return fmt.Errorf("cocoon image pull %s: %w (output: %s)", image, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Inspect runs `cocoon vm inspect <ref>`.
func (c *CocoonCLI) Inspect(ctx context.Context, vmID string) (*VM, error) {
	out, err := c.runJSON(ctx, "vm", "inspect", vmID)
	if err != nil {
		return nil, fmt.Errorf("cocoon vm inspect %s: %w", vmID, err)
	}
	return parseInspectJSON(out)
}

// List runs `cocoon vm list -o json` and returns every known VM.
func (c *CocoonCLI) List(ctx context.Context) ([]VM, error) {
	out, err := c.runJSON(ctx, "vm", "list", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("cocoon vm list: %w", err)
	}
	return parseVMListJSON(out)
}

// Remove runs `cocoon vm rm --force <id>`.
func (c *CocoonCLI) Remove(ctx context.Context, vmID string) error {
	cmd := c.command(ctx, "vm", "rm", "--force", vmID)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cocoon vm rm %s: %w (output: %s)", vmID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SnapshotSave runs `cocoon snapshot save --name <vmName> <vmID>`.
func (c *CocoonCLI) SnapshotSave(ctx context.Context, vmName, vmID string) error {
	cmd := c.command(ctx, "snapshot", "save", "--name", vmName, vmID)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cocoon snapshot save %s: %w (output: %s)", vmName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Snapshot runs `cocoon snapshot inspect <name>`.
func (c *CocoonCLI) Snapshot(ctx context.Context, name string) (*Snapshot, error) {
	out, err := c.runJSON(ctx, "snapshot", "inspect", name)
	if err != nil {
		return nil, fmt.Errorf("cocoon snapshot inspect %s: %w", name, err)
	}
	return parseSnapshotJSON(out)
}

// SnapshotImport spawns `cocoon snapshot import --name <name>` and
// returns its stdin pipe so callers can stream tar bytes in. The
// returned wait function blocks until the subprocess exits.
func (c *CocoonCLI) SnapshotImport(ctx context.Context, opts ImportOptions) (io.WriteCloser, func() error, error) {
	args := []string{"snapshot", "import", "--name", opts.Name}
	if opts.Description != "" {
		args = append(args, "--description", opts.Description)
	}
	cmd := c.command(ctx, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nil, fmt.Errorf("start cocoon snapshot import: %w", err)
	}
	wait := func() error {
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("cocoon snapshot import: %w", err)
		}
		return nil
	}
	return stdin, wait, nil
}

// SnapshotExport spawns `cocoon snapshot export <name> -o -` and
// returns its stdout pipe.
func (c *CocoonCLI) SnapshotExport(ctx context.Context, vmName string) (io.ReadCloser, func() error, error) {
	cmd := c.command(ctx, "snapshot", "export", vmName, "-o", "-")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("start cocoon snapshot export: %w", err)
	}
	wait := func() error {
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("cocoon snapshot export: %w", err)
		}
		return nil
	}
	return stdout, wait, nil
}

// runJSON runs cocoon and returns its stdout as raw JSON bytes.
func (c *CocoonCLI) runJSON(ctx context.Context, args ...string) ([]byte, error) {
	cmd := c.command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// appendCreateArgs adds the resource / network / nics / dns flags
// shared by cocoon vm run and cocoon vm clone.
func appendCreateArgs(args []string, cpu int, memory, network, storage string, nics int, dns []string) []string {
	if cpu > 0 {
		args = append(args, "--cpu", strconv.Itoa(cpu))
	}
	if normalized := normalizeSizeArg(memory); normalized != "" {
		args = append(args, "--memory", normalized)
	}
	if normalized := normalizeSizeArg(storage); normalized != "" {
		args = append(args, "--storage", normalized)
	}
	if network != "" {
		args = append(args, "--network", network)
	}
	if nics > 0 {
		args = append(args, "--nics", strconv.Itoa(nics))
	}
	if len(dns) > 0 {
		args = append(args, "--dns", strings.Join(dns, ","))
	}
	return args
}

// normalizeSizeArg converts Kubernetes-style quantities (for example "20Gi")
// into plain byte counts, which cocoon's CLI parser accepts reliably.
func normalizeSizeArg(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return raw
	}
	if bytes := q.Value(); bytes > 0 {
		return strconv.FormatInt(bytes, 10)
	}
	return raw
}
