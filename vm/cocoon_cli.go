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

	"k8s.io/apimachinery/pkg/api/resource"
)

// cocoon CLI binary path and backend name constants.
const (
	defaultCocoonBinary = "/usr/local/bin/cocoon"

	// BackendFirecracker matches cocoonv1.BackendFirecracker. Exported so
	// provider/cocoon can reuse it without importing CRD types.
	BackendFirecracker = "firecracker"
)

var _ Runtime = (*CocoonCLI)(nil)

// CocoonCLI is the production Runtime that shells out to `cocoon`.
type CocoonCLI struct {
	binary string
	sudo   bool
}

// NewCocoonCLI returns a CocoonCLI; empty binary resolves to defaultCocoonBinary.
func NewCocoonCLI(binary string, sudo bool) *CocoonCLI {
	if binary == "" {
		binary = defaultCocoonBinary
	}
	return &CocoonCLI{binary: binary, sudo: sudo}
}

// Clone runs `cocoon vm clone`.
func (c *CocoonCLI) Clone(ctx context.Context, opts CloneOptions) (*VM, error) {
	args := buildCloneArgs(opts)
	if _, err := c.runJSON(ctx, args...); err != nil {
		return nil, fmt.Errorf("cocoon vm clone: %w", err)
	}
	return c.Inspect(ctx, opts.To)
}

// Run runs `cocoon vm run`.
func (c *CocoonCLI) Run(ctx context.Context, opts RunOptions) (*VM, error) {
	if err := c.EnsureImage(ctx, opts.Image, opts.Force); err != nil {
		return nil, fmt.Errorf("ensure image %s: %w", opts.Image, err)
	}

	args := buildRunArgs(opts)
	if _, err := c.runJSON(ctx, args...); err != nil {
		return nil, fmt.Errorf("cocoon vm run: %w", err)
	}
	return c.Inspect(ctx, opts.Name)
}

// EnsureImage ensures the image is available locally and up to date.
// It always attempts a pull so that mutable tags (e.g. ":latest") pick up
// upstream replacements. The pull itself is idempotent — cocoon skips the
// download when the local content already matches the remote digest.
// When force is true, --force is passed to cocoon to bypass the cache
// entirely and re-download from upstream.
func (c *CocoonCLI) EnsureImage(ctx context.Context, image string, force bool) error {
	if image == "" {
		return nil
	}
	args := []string{"image", "pull"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, image)
	out, err := c.command(ctx, args...).CombinedOutput()
	if err != nil {
		return cocoonCmdError("image pull", image, err, out)
	}
	return nil
}

// Inspect runs `cocoon vm inspect`.
func (c *CocoonCLI) Inspect(ctx context.Context, vmID string) (*VM, error) {
	out, err := c.runJSON(ctx, "vm", "inspect", vmID)
	if err != nil {
		return nil, fmt.Errorf("cocoon vm inspect %s: %w", vmID, err)
	}
	return parseInspectJSON(out)
}

// List runs `cocoon vm list`.
func (c *CocoonCLI) List(ctx context.Context) ([]VM, error) {
	out, err := c.runJSON(ctx, "vm", "list", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("cocoon vm list: %w", err)
	}
	return parseVMListJSON(out)
}

// Remove runs `cocoon vm rm --force`.
func (c *CocoonCLI) Remove(ctx context.Context, vmID string) error {
	cmd := c.command(ctx, "vm", "rm", "--force", vmID)
	if out, err := cmd.CombinedOutput(); err != nil {
		return cocoonCmdError("vm rm", vmID, err, out)
	}
	return nil
}

// SnapshotSave runs `cocoon snapshot save`, handling "already exists" idempotently.
// The v-k workqueue retries UpdatePod rapidly, and a crashed hibernate can leave
// a stale snapshot that blocks every retry. When "already exists" is detected,
// we rm the stale snapshot and re-issue save.
func (c *CocoonCLI) SnapshotSave(ctx context.Context, vmName, vmID string) error {
	out, err := c.command(ctx, "snapshot", "save", "--name", vmName, vmID).CombinedOutput()
	if err == nil {
		return nil
	}
	if !strings.Contains(string(out), "already exists") {
		return cocoonCmdError("snapshot save", vmName, err, out)
	}
	rmOut, rmErr := c.command(ctx, "snapshot", "rm", vmName).CombinedOutput()
	if rmErr != nil {
		return fmt.Errorf("cocoon snapshot save %s: stale snapshot present and rm failed: %w (output: %s)", vmName, rmErr, strings.TrimSpace(string(rmOut)))
	}
	out2, err2 := c.command(ctx, "snapshot", "save", "--name", vmName, vmID).CombinedOutput()
	if err2 != nil {
		return cocoonCmdError("snapshot save (after rm)", vmName, err2, out2)
	}
	return nil
}

// Snapshot runs `cocoon snapshot inspect`.
func (c *CocoonCLI) Snapshot(ctx context.Context, name string) (*Snapshot, error) {
	out, err := c.runJSON(ctx, "snapshot", "inspect", name)
	if err != nil {
		return nil, fmt.Errorf("cocoon snapshot inspect %s: %w", name, err)
	}
	return parseSnapshotJSON(out)
}

// SnapshotImport spawns `cocoon snapshot import` and returns its stdin pipe.
// Stale snapshots at the same name are removed up-front for idempotency
// (same retry-loop reasoning as SnapshotSave).
func (c *CocoonCLI) SnapshotImport(ctx context.Context, opts ImportOptions) (io.WriteCloser, func() error, error) {
	if err := c.snapshotRemoveIfExists(ctx, opts.Name); err != nil {
		return nil, nil, err
	}
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

// SnapshotExport spawns `cocoon snapshot export` and returns its stdout pipe.
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

// Start runs `cocoon vm start`.
func (c *CocoonCLI) Start(ctx context.Context, vmID string) error {
	out, err := c.command(ctx, "vm", "start", vmID).CombinedOutput()
	if err != nil {
		return cocoonCmdError("vm start", vmID, err, out)
	}
	return nil
}

// WatchEvents starts `cocoon vm status --event --format json` and streams
// parsed VMEvent values. The channel closes when ctx is canceled or the
// subprocess exits. On parse errors the line is silently skipped.
func (c *CocoonCLI) WatchEvents(ctx context.Context) (<-chan VMEvent, error) {
	cmd := c.command(ctx, "vm", "status", "--event", "--format", "json")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cocoon vm status: %w", err)
	}
	ch := make(chan VMEvent, 16)
	go func() {
		defer close(ch)
		defer cmd.Wait() //nolint:errcheck
		dec := json.NewDecoder(stdout)
		for {
			var raw struct {
				Event string          `json:"event"`
				VM    json.RawMessage `json:"vm"`
			}
			if err := dec.Decode(&raw); err != nil {
				return // EOF or ctx canceled
			}
			ev := VMEvent{Event: raw.Event}
			ev.VM = parseVMFromStatusJSON(raw.VM)
			if ev.VM.ID == "" && ev.VM.Name == "" {
				continue
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// buildCloneArgs assembles the cocoon vm clone argv. Firecracker restores
// the entire VM state from the snapshot and cannot resize CPU/memory on
// load, so those overrides are stripped when targeting firecracker. Extracted
// for direct unit-test coverage.
func buildCloneArgs(opts CloneOptions) []string {
	args := []string{"vm", "clone"}
	if opts.To != "" {
		args = append(args, "--name", opts.To)
	}
	cpu, memory := opts.CPU, opts.Memory
	if opts.Backend == BackendFirecracker {
		cpu, memory = 0, ""
	}
	args = appendCreateArgs(args, cpu, memory, opts.Network, opts.Storage, opts.NICs, opts.DNS)
	if opts.NoDirectIO {
		args = append(args, "--no-direct-io")
	}
	if opts.Pull {
		args = append(args, "--pull")
	}
	args = append(args, opts.From)
	return args
}

// buildRunArgs assembles the cocoon vm run argv. Extracted for direct
// unit-test coverage of the backend / OS flag fan-out.
func buildRunArgs(opts RunOptions) []string {
	args := []string{"vm", "run"}
	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}
	args = appendCreateArgs(args, opts.CPU, opts.Memory, opts.Network, opts.Storage, opts.NICs, opts.DNS)
	if strings.EqualFold(opts.OS, "windows") {
		args = append(args, "--windows")
	}
	if opts.Backend == BackendFirecracker {
		args = append(args, "--fc")
	}
	if opts.NoDirectIO {
		args = append(args, "--no-direct-io")
	}
	args = append(args, opts.Image)
	return args
}

// command builds an exec.Cmd, optionally wrapped in sudo.
func (c *CocoonCLI) command(ctx context.Context, args ...string) *exec.Cmd {
	if c.sudo {
		full := append([]string{c.binary}, args...)
		return exec.CommandContext(ctx, "sudo", full...) //nolint:gosec // path comes from operator config, not untrusted input
	}
	return exec.CommandContext(ctx, c.binary, args...) //nolint:gosec // see above
}

// parseVMFromStatusJSON extracts ID, Name, State from the vm status JSON.
func parseVMFromStatusJSON(data []byte) VM {
	var obj struct {
		ID     string `json:"id"`
		Config struct {
			Name string `json:"name"`
		} `json:"config"`
		State          string `json:"state"`
		NetworkConfigs []struct {
			Mac     string `json:"mac"`
			Network *struct {
				IP string `json:"ip"`
			} `json:"network"`
		} `json:"network_configs"`
	}
	if json.Unmarshal(data, &obj) != nil {
		return VM{}
	}
	v := VM{
		ID:    obj.ID,
		Name:  obj.Config.Name,
		State: obj.State,
	}
	if len(obj.NetworkConfigs) > 0 {
		v.MAC = obj.NetworkConfigs[0].Mac
		if obj.NetworkConfigs[0].Network != nil {
			v.IP = obj.NetworkConfigs[0].Network.IP
		}
	}
	return v
}

// snapshotRemoveIfExists drops a snapshot by name, treating "not found" as success.
func (c *CocoonCLI) snapshotRemoveIfExists(ctx context.Context, name string) error {
	out, err := c.command(ctx, "snapshot", "rm", name).CombinedOutput()
	if err == nil {
		return nil
	}
	if strings.Contains(string(out), "snapshot not found") {
		return nil
	}
	return cocoonCmdError("snapshot rm", name, err, out)
}

// runJSON runs cocoon and returns stdout as raw JSON.
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

// cocoonCmdError formats a consistent error message for cocoon subprocess failures.
func cocoonCmdError(op, ref string, err error, output []byte) error {
	return fmt.Errorf("cocoon %s %s: %w (output: %s)", op, ref, err, strings.TrimSpace(string(output)))
}

// appendCreateArgs adds resource/network flags shared by clone and run.
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

// normalizeSizeArg converts K8s quantities (e.g. "20Gi") to plain byte counts.
func normalizeSizeArg(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return raw
	}
	if n := q.Value(); n > 0 {
		return strconv.FormatInt(n, 10)
	}
	return raw
}
