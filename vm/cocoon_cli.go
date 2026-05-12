// Package vm wraps the cocoon CLI for VM lifecycle operations.
//
// This package is the cocoon CLI bridge; subprocess calls are architectural,
// not tech debt. cocoon is the authoritative VM controller and exposes its
// contract through the CLI, so vk-cocoon shells out rather than linking
// against cocoon's internals.
package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/projecteru2/core/log"
	"k8s.io/apimachinery/pkg/api/resource"
	utilexec "k8s.io/client-go/util/exec"
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

// Clone runs `cocoon vm clone --output json` and parses the emitted VM
// record directly, avoiding a second inspect round trip.
func (c *CocoonCLI) Clone(ctx context.Context, opts CloneOptions) (*VM, error) {
	out, err := c.runJSON(ctx, buildCloneArgs(opts)...)
	if err != nil {
		return nil, fmt.Errorf("cocoon vm clone: %w", err)
	}
	v, err := parseInspectJSON(out)
	if err != nil {
		return nil, fmt.Errorf("cocoon vm clone: %w", err)
	}
	if v.ID == "" {
		return nil, fmt.Errorf("cocoon vm clone %s: empty VM record in JSON payload", opts.To)
	}
	return v, nil
}

// Run runs `cocoon vm run --output json`; cocoon re-inspects after start
// so the emitted JSON reflects the running state (PID, IP). If cocoon's
// own post-start inspect failed it falls back to the pre-start record
// (State!="running", PID=0) and only warns on stderr — detect that here
// and do a single make-up Inspect so callers always see live state.
// Caller must have ensured the image locally before invoking Run.
func (c *CocoonCLI) Run(ctx context.Context, opts RunOptions) (*VM, error) {
	out, err := c.runJSON(ctx, buildRunArgs(opts)...)
	if err != nil {
		return nil, fmt.Errorf("cocoon vm run: %w", err)
	}
	v, err := parseInspectJSON(out)
	if err != nil {
		return nil, fmt.Errorf("cocoon vm run: %w", err)
	}
	if v.ID == "" {
		return nil, fmt.Errorf("cocoon vm run %s: empty VM record in JSON payload", opts.Name)
	}
	if v.State != StateRunning || v.PID == 0 {
		return c.Inspect(ctx, v.ID)
	}
	return v, nil
}

// EnsureImage shells `cocoon image pull`; force=true adds --force.
// Cocoonstack cloud-image artifacts must go through Puller.EnsureCloudImage
// instead — `cocoon image pull` mistakes them for container images.
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

// Image runs `cocoon image inspect`; "not found in any backend" maps to ErrImageNotFound.
func (c *CocoonCLI) Image(ctx context.Context, name string) (*Image, error) {
	if name == "" {
		return nil, fmt.Errorf("cocoon image inspect: name is empty")
	}
	out, err := c.command(ctx, "image", "inspect", name).CombinedOutput()
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)), "not found in any backend") {
			return nil, fmt.Errorf("cocoon image inspect %s: %w", name, ErrImageNotFound)
		}
		return nil, cocoonCmdError("image inspect", name, err, out)
	}
	return &Image{Name: name}, nil
}

// ImageImport spawns `cocoon image import <name>` and returns its stdin
// pipe. Mirrors SnapshotImport; cocoon auto-detects qcow2 vs tar.
func (c *CocoonCLI) ImageImport(ctx context.Context, opts ImageImportOptions) (io.WriteCloser, func() error, error) {
	if opts.Name == "" {
		return nil, nil, fmt.Errorf("cocoon image import: name is empty")
	}
	cmd := c.command(ctx, "image", "import", opts.Name)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nil, fmt.Errorf("start cocoon image import: %w", err)
	}
	wait := func() error {
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("cocoon image import: %w", err)
		}
		return nil
	}
	return stdin, wait, nil
}

// Inspect runs `cocoon vm inspect`; cocoon's "not found" maps to ErrVMNotFound.
// Any other error is inconclusive (transient CLI failure, sudo timeout, etc.).
func (c *CocoonCLI) Inspect(ctx context.Context, vmID string) (*VM, error) {
	out, err := c.runJSON(ctx, "vm", "inspect", vmID)
	if err != nil {
		if isCocoonNotFound(err) {
			return nil, fmt.Errorf("cocoon vm inspect %s: %w", vmID, ErrVMNotFound)
		}
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

// Exec runs `cocoon vm exec`. Non-zero child exit → utilexec.CodeExitError
// (vk's RemoteCommand handler probes that interface for the kubectl status);
// transport / setup failures bubble up as plain errors.
func (c *CocoonCLI) Exec(ctx context.Context, vmID string, argv []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) error {
	if vmID == "" {
		return errors.New("cocoon vm exec: vmID is empty")
	}
	if len(argv) == 0 {
		return errors.New("cocoon vm exec: argv is empty")
	}
	cmd := c.command(ctx, buildExecArgs(vmID, argv, env)...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return utilexec.CodeExitError{Err: fmt.Errorf("cocoon vm exec %s: exit %d", vmID, exitErr.ExitCode()), Code: exitErr.ExitCode()}
		}
		return fmt.Errorf("cocoon vm exec %s: %w", vmID, err)
	}
	return nil
}

// Remove runs `cocoon vm rm --force`.
func (c *CocoonCLI) Remove(ctx context.Context, vmID string) error {
	cmd := c.command(ctx, "vm", "rm", "--force", vmID)
	if out, err := cmd.CombinedOutput(); err != nil {
		return cocoonCmdError("vm rm", vmID, err, out)
	}
	return nil
}

// Logs runs `cocoon vm logs [--tail N] <vmID>` and returns the hypervisor log.
// stdout and stderr are captured separately so cocoon's diagnostic output
// is surfaced in errors instead of leaking into the log stream.
func (c *CocoonCLI) Logs(ctx context.Context, vmID string, tail int) (io.ReadCloser, error) {
	if vmID == "" {
		return nil, errors.New("cocoon vm logs: vmID is empty")
	}
	args := []string{"vm", "logs"}
	if tail > 0 {
		args = append(args, "--tail", strconv.Itoa(tail))
	}
	args = append(args, vmID)
	cmd := c.command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, cocoonCmdError("vm logs", vmID, err, stderr.Bytes())
	}
	return io.NopCloser(&stdout), nil
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
	if err := c.SnapshotRemoveIfExists(ctx, opts.Name); err != nil {
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

// SnapshotRemoveIfExists drops a snapshot by name, treating "not found" as
// success. Exposed so callers can invalidate cached fork snapshots when a
// main VM is recreated.
func (c *CocoonCLI) SnapshotRemoveIfExists(ctx context.Context, name string) error {
	out, err := c.command(ctx, "snapshot", "rm", name).CombinedOutput()
	if err == nil {
		return nil
	}
	if strings.Contains(string(out), "snapshot not found") {
		return nil
	}
	return cocoonCmdError("snapshot rm", name, err, out)
}

// Start runs `cocoon vm start`.
func (c *CocoonCLI) Start(ctx context.Context, vmID string) error {
	out, err := c.command(ctx, "vm", "start", vmID).CombinedOutput()
	if err != nil {
		return cocoonCmdError("vm start", vmID, err, out)
	}
	return nil
}

// String-matched on stderr — cocoon CLI has no structured error channel.
const netResizeUnsupportedMarker = "backend does not support net resize"

// NetResize runs `cocoon vm net --nics N`.
func (c *CocoonCLI) NetResize(ctx context.Context, vmID string, target int) error {
	out, err := c.command(ctx, "vm", "net", "--nics", strconv.Itoa(target), vmID).CombinedOutput()
	if err != nil {
		if bytes.Contains(out, []byte(netResizeUnsupportedMarker)) {
			return ErrNetResizeUnsupported
		}
		return cocoonCmdError("vm net", vmID, err, out)
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
		defer cmd.Wait() //nolint:errcheck // wait error ignored on goroutine cleanup path
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

// buildCloneArgs assembles the cocoon vm clone argv.
func buildCloneArgs(opts CloneOptions) []string {
	args := []string{"vm", "clone", "--output", "json"}
	if opts.To != "" {
		args = append(args, "--name", opts.To)
	}
	if opts.Network != "" {
		args = append(args, "--network", opts.Network)
	}
	if opts.NoDirectIO {
		args = append(args, "--no-direct-io")
	}
	// FromDir forces --pull because the dir holds no base image layers.
	if opts.Pull || opts.FromDir != "" {
		args = append(args, "--pull")
	}
	if opts.OnDemand && opts.Backend != BackendFirecracker {
		// UFFD lazy memory restore is CH-only; skipping on FC keeps the
		// same CloneOptions usable for both backends.
		args = append(args, "--on-demand")
	}
	if opts.NICs != nil {
		args = append(args, "--nics", strconv.Itoa(*opts.NICs))
	}
	if opts.FromDir != "" {
		return append(args, "--from-dir", opts.FromDir)
	}
	return append(args, opts.From)
}

// buildRunArgs assembles the cocoon vm run argv.
func buildRunArgs(opts RunOptions) []string {
	args := []string{"vm", "run", "--output", "json"}
	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}
	if opts.CPU > 0 {
		args = append(args, "--cpu", strconv.Itoa(opts.CPU))
	}
	if memory := normalizeSizeArg(opts.Memory); memory != "" {
		args = append(args, "--memory", memory)
	}
	if storage := normalizeSizeArg(opts.Storage); storage != "" {
		args = append(args, "--storage", storage)
	}
	if opts.Network != "" {
		args = append(args, "--network", opts.Network)
	}
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

// buildExecArgs assembles `cocoon vm exec [-e KEY=VAL...] <vmID> -- <argv>...`.
// Env keys are sorted so the resulting argv is deterministic (test-friendly,
// log-friendly); cocoon doesn't care about order.
func buildExecArgs(vmID string, argv []string, env map[string]string) []string {
	args := make([]string, 0, 4+2*len(env)+len(argv)) //nolint:mnd
	args = append(args, "vm", "exec")
	for _, k := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(args, vmID, "--")
	args = append(args, argv...)
	return args
}

// command builds an exec.Cmd, optionally wrapped in sudo.
// Every invocation is logged at debug so operators can see the external
// binary surface — see the package doc for why the subprocess boundary
// exists.
func (c *CocoonCLI) command(ctx context.Context, args ...string) *exec.Cmd {
	log.WithFunc("vm.CocoonCLI.command").Debugf(ctx, "exec cocoon: %v", args)
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

// isCocoonNotFound detects cocoon's VM-not-found signal inside the
// stderr-embedded wrapped error produced by runJSON. Restricted to
// VM-specific phrases so an unrelated binary/config "not found" stderr
// cannot be promoted to an authoritative VMGone.
func isCocoonNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "vm not found") ||
		strings.Contains(s, "no such vm")
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
