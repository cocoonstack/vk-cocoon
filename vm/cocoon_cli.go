package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

const (
	// defaultCocoonBinary is the path used when COCOON_BIN is unset.
	defaultCocoonBinary = "/usr/local/bin/cocoon"
)

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

// Clone runs `cocoon vm clone --from <source> --to <name>`.
func (c *CocoonCLI) Clone(ctx context.Context, opts CloneOptions) (*VM, error) {
	args := []string{"vm", "clone", "--from", opts.From, "--to", opts.To, "--json"}
	args = appendCommonArgs(args, opts.Network, opts.Storage, opts.NICs, opts.DNS)
	out, err := c.runJSON(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("cocoon vm clone: %w", err)
	}
	return parseInspectJSON(out)
}

// Run runs `cocoon run --image <url>`.
func (c *CocoonCLI) Run(ctx context.Context, opts RunOptions) (*VM, error) {
	args := []string{"run", "--image", opts.Image, "--name", opts.Name, "--json"}
	args = appendCommonArgs(args, opts.Network, opts.Storage, opts.NICs, opts.DNS)
	if opts.OS != "" {
		args = append(args, "--os", opts.OS)
	}
	out, err := c.runJSON(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("cocoon run: %w", err)
	}
	return parseInspectJSON(out)
}

// Inspect runs `cocoon vm inspect <id> --json`.
func (c *CocoonCLI) Inspect(ctx context.Context, vmID string) (*VM, error) {
	out, err := c.runJSON(ctx, "vm", "inspect", vmID, "--json")
	if err != nil {
		return nil, fmt.Errorf("cocoon vm inspect %s: %w", vmID, err)
	}
	return parseInspectJSON(out)
}

// List runs `cocoon vm list --json` and returns every known VM.
func (c *CocoonCLI) List(ctx context.Context) ([]VM, error) {
	out, err := c.runJSON(ctx, "vm", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("cocoon vm list: %w", err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("decode vm list: %w", err)
	}
	out2 := make([]VM, 0, len(raw))
	for _, m := range raw {
		vm, perr := vmFromMap(m)
		if perr != nil {
			continue
		}
		out2 = append(out2, *vm)
	}
	return out2, nil
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

// appendCommonArgs adds the network / storage / nics / dns flags
// every clone or run command shares.
func appendCommonArgs(args []string, network, storage string, nics int, dns []string) []string {
	if network != "" {
		args = append(args, "--network", network)
	}
	if storage != "" {
		args = append(args, "--storage", storage)
	}
	if nics > 0 {
		args = append(args, "--nics", strconv.Itoa(nics))
	}
	if len(dns) > 0 {
		args = append(args, "--dns", strings.Join(dns, ","))
	}
	return args
}
