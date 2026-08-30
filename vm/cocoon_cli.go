// Package vm wraps the cocoon CLI for VM lifecycle operations.
// Subprocess calls are the deliberate architecture: cocoon is the authoritative VM controller, so vk-cocoon shells out rather than linking against its internals.
package vm

import (
	"bufio"
	"bytes"
	"cmp"
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
	"time"

	"github.com/projecteru2/core/log"
	"k8s.io/apimachinery/pkg/api/resource"
	utilexec "k8s.io/client-go/util/exec"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
)

const (
	defaultCocoonBinary = "/usr/local/bin/cocoon"

	// BackendFirecracker matches cocoonv1.BackendFirecracker; exported for provider/cocoon to reuse without importing CRD types.
	BackendFirecracker = "firecracker"

	// maxEventLineBytes bounds one `cocoon vm status --event` JSON line.
	maxEventLineBytes = 1 << 20

	// staleSnapshotRmBudget / staleSnapshotRmDelay bound the wait for a killed save's orphaned child to release the snapshot flock.
	staleSnapshotRmBudget = 30 * time.Second
	staleSnapshotRmDelay  = 500 * time.Millisecond
)

var (
	// snapshotNameTaken are cocoon's two same-name rejection phrases for a killed save's still-held name.
	snapshotNameTaken = []string{"already exists", "already in use by"}

	// snapshotLeaseHeld is cocoon's rm refusal while the snapshot's flock is still held.
	snapshotLeaseHeld = []string{"is in use by an active"}
)

var _ Runtime = (*CocoonCLI)(nil)

// CocoonCLI is the production Runtime that shells out to `cocoon`.
type CocoonCLI struct {
	binary string
}

// NewCocoonCLI returns a CocoonCLI; empty binary → defaultCocoonBinary. For non-root setups, point binary at a wrapper or setcap the cocoon binary.
func NewCocoonCLI(binary string) *CocoonCLI {
	return &CocoonCLI{binary: cmp.Or(binary, defaultCocoonBinary)}
}

// Clone runs `cocoon vm clone --output json` and parses the emitted VM record, avoiding a second inspect round trip.
func (c *CocoonCLI) Clone(ctx context.Context, opts CloneOptions) (*VM, error) {
	return c.runAndParseVM(ctx, "cocoon vm clone", opts.To, buildCloneArgs(opts))
}

// Run runs `cocoon vm run --output json`; if cocoon's post-start inspect failed
// (State!="running", PID=0) this does a make-up Inspect so callers always see live state. Caller must have ensured the image locally.
func (c *CocoonCLI) Run(ctx context.Context, opts RunOptions) (*VM, error) {
	v, err := c.runAndParseVM(ctx, "cocoon vm run", opts.Name, buildRunArgs(opts))
	if err != nil {
		return nil, err
	}
	if v.State != StateRunning || v.PID == 0 {
		return c.Inspect(ctx, v.ID)
	}
	return v, nil
}

// EnsureImage shells `cocoon image pull` (force=true adds --force); cloud-image artifacts must instead go through Puller.EnsureCloudImageFromRaw.
func (c *CocoonCLI) EnsureImage(ctx context.Context, image string, force bool) error {
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

// Image runs `cocoon image inspect` as a local-presence probe; "not found in any backend" maps to ErrImageNotFound.
func (c *CocoonCLI) Image(ctx context.Context, name string) error {
	out, err := c.command(ctx, "image", "inspect", name).CombinedOutput()
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)), "not found in any backend") {
			return fmt.Errorf("cocoon image inspect %s: %w", name, ErrImageNotFound)
		}
		return cocoonCmdError("image inspect", name, err, out)
	}
	return nil
}

// ImageImport spawns `cocoon image import <name>` and returns its stdin pipe; cocoon auto-detects qcow2 vs tar.
func (c *CocoonCLI) ImageImport(ctx context.Context, name string) (io.WriteCloser, func() error, error) {
	cmd := c.command(ctx, "image", "import", name)
	return startCmdPipe(ctx, cmd, cmd.StdinPipe, "cocoon image import")
}

// Inspect runs `cocoon vm inspect`; cocoon's "not found" maps to ErrVMNotFound, any other error is inconclusive.
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

// ReconcileStaleCreate runs `cocoon vm reconcile-stale-create`.
func (c *CocoonCLI) ReconcileStaleCreate(ctx context.Context, vmID string) (StaleCreateOutcome, error) {
	out, err := c.runJSON(ctx, "vm", "reconcile-stale-create", vmID, "-o", "json")
	if err != nil {
		return "", fmt.Errorf("cocoon vm reconcile-stale-create %s: %w", vmID, err)
	}
	var res struct {
		Outcome StaleCreateOutcome `json:"outcome"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", fmt.Errorf("cocoon vm reconcile-stale-create %s: %w", vmID, err)
	}
	if res.Outcome == "" {
		return "", fmt.Errorf("cocoon vm reconcile-stale-create %s: empty outcome in JSON payload", vmID)
	}
	return res.Outcome, nil
}

// Exec runs `cocoon vm exec`; non-zero child exit maps to utilexec.CodeExitError for vk's RemoteCommand status probe.
func (c *CocoonCLI) Exec(ctx context.Context, vmID string, argv []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) error {
	if vmID == "" {
		return errors.New("cocoon vm exec: vmID is empty")
	}
	if len(argv) == 0 {
		return errors.New("cocoon vm exec: argv is empty")
	}
	cmd := c.command(ctx, buildExecArgs(vmID, argv, env, stdin != nil)...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
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

// Logs runs `cocoon vm logs [--tail N] <vmID>`; stdout and stderr are captured separately so diagnostics surface in errors, not the log stream.
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

// SnapshotSave runs `cocoon snapshot save`, dropping a name a crashed hibernate still holds before retrying.
func (c *CocoonCLI) SnapshotSave(ctx context.Context, vmName, vmID string) error {
	out, err := c.command(ctx, "snapshot", "save", "--name", vmName, vmID).CombinedOutput()
	if err == nil {
		return nil
	}
	if !containsAny(string(out), snapshotNameTaken...) {
		return cocoonCmdError("snapshot save", vmName, err, out)
	}
	holder := cmp.Or(snapshotNameHolderID(string(out)), vmName)
	if rmErr := c.removeStaleSnapshot(ctx, holder); rmErr != nil {
		return fmt.Errorf("cocoon snapshot save %s: name held by %s: %w", vmName, holder, rmErr)
	}
	out2, err2 := c.command(ctx, "snapshot", "save", "--name", vmName, vmID).CombinedOutput()
	if err2 != nil {
		return cocoonCmdError("snapshot save (after rm)", vmName, err2, out2)
	}
	return nil
}

// Snapshot runs `cocoon snapshot inspect`; "snapshot not found" maps to ErrSnapshotNotFound.
func (c *CocoonCLI) Snapshot(ctx context.Context, name string) (*Snapshot, error) {
	out, err := c.runJSON(ctx, "snapshot", "inspect", name)
	if err != nil {
		if isCocoonSnapshotNotFound(err) {
			return nil, fmt.Errorf("cocoon snapshot inspect %s: %w", name, ErrSnapshotNotFound)
		}
		return nil, fmt.Errorf("cocoon snapshot inspect %s: %w", name, err)
	}
	return parseSnapshotJSON(out)
}

// SnapshotImport spawns `cocoon snapshot import` and returns its stdin pipe, removing a stale same-name snapshot up front for idempotency.
func (c *CocoonCLI) SnapshotImport(ctx context.Context, name string) (io.WriteCloser, func() error, error) {
	if err := c.SnapshotRemoveIfExists(ctx, name); err != nil {
		return nil, nil, err
	}
	cmd := c.command(ctx, "snapshot", "import", "--name", name)
	return startCmdPipe(ctx, cmd, cmd.StdinPipe, "cocoon snapshot import")
}

// SnapshotExport spawns `cocoon snapshot export` and returns its stdout pipe.
func (c *CocoonCLI) SnapshotExport(ctx context.Context, vmName string) (io.ReadCloser, func() error, error) {
	cmd := c.command(ctx, "snapshot", "export", vmName, "-o", "-")
	return startCmdPipe(ctx, cmd, cmd.StdoutPipe, "cocoon snapshot export")
}

// SnapshotRemoveIfExists drops a snapshot by name, treating "not found" as success.
func (c *CocoonCLI) SnapshotRemoveIfExists(ctx context.Context, name string) error {
	out, err := c.command(ctx, "snapshot", "rm", name).CombinedOutput()
	if err == nil {
		return nil
	}
	wrapped := cocoonCmdError("snapshot rm", name, err, out)
	if isCocoonSnapshotNotFound(wrapped) {
		return nil
	}
	return wrapped
}

// Start runs `cocoon vm start`.
func (c *CocoonCLI) Start(ctx context.Context, vmID string) error {
	out, err := c.command(ctx, "vm", "start", vmID).CombinedOutput()
	if err != nil {
		return cocoonCmdError("vm start", vmID, err, out)
	}
	return nil
}

// NetResize runs `cocoon vm net --nics N`.
func (c *CocoonCLI) NetResize(ctx context.Context, vmID string, target int) error {
	out, err := c.command(ctx, "vm", "net", "--nics", strconv.Itoa(target), vmID).CombinedOutput()
	if err != nil {
		return cocoonCmdError("vm net", vmID, err, out)
	}
	return nil
}

// WatchEvents streams parsed VMEvent values from `cocoon vm status --event --format json`; an undecodable line is logged and skipped rather than killing the stream.
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
		logger := log.WithFunc("vm.CocoonCLI.WatchEvents")
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), maxEventLineBytes)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var raw struct {
				Event string          `json:"event"`
				VM    json.RawMessage `json:"vm"`
			}
			if err := json.Unmarshal(line, &raw); err != nil {
				logger.Warnf(ctx, "skip undecodable event line: %v", err)
				continue
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
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			logger.Warnf(ctx, "event stream read: %v", err)
		}
	}()
	return ch, nil
}

// removeStaleSnapshot rms the name holder, retrying only the lease-held refusal until the orphaned child dies.
func (c *CocoonCLI) removeStaleSnapshot(ctx context.Context, ref string) error {
	deadline := time.Now().Add(staleSnapshotRmBudget)
	for {
		out, err := c.command(ctx, "snapshot", "rm", ref).CombinedOutput()
		if err == nil {
			return nil
		}
		trimmed := strings.TrimSpace(string(out))
		if !containsAny(trimmed, snapshotLeaseHeld...) {
			return fmt.Errorf("rm failed: %w (output: %s)", err, trimmed)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("rm still lease-blocked after %s: %w (output: %s)", staleSnapshotRmBudget, err, trimmed)
		}
		log.WithFunc("vm.CocoonCLI.removeStaleSnapshot").
			Debugf(ctx, "snapshot %s lease held, retrying rm in %s", ref, staleSnapshotRmDelay)
		if !commonk8s.SleepCtx(ctx, staleSnapshotRmDelay) {
			return ctx.Err()
		}
	}
}

// command builds an exec.Cmd; logged at debug for operator visibility into the external binary surface.
func (c *CocoonCLI) command(ctx context.Context, args ...string) *exec.Cmd {
	log.WithFunc("vm.CocoonCLI.command").Debugf(ctx, "exec cocoon: %v", args)
	return exec.CommandContext(ctx, c.binary, args...) //nolint:gosec // path comes from operator config, not untrusted input
}

// runAndParseVM runs a VM-emitting verb and rejects an empty record.
func (c *CocoonCLI) runAndParseVM(ctx context.Context, op, ref string, args []string) (*VM, error) {
	out, err := c.runJSON(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	v, err := parseInspectJSON(out)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if v.ID == "" {
		return nil, fmt.Errorf("%s %s: empty VM record in JSON payload", op, ref)
	}
	return v, nil
}

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

// NormalizeSizeArg converts K8s quantities (e.g. "20Gi") to plain byte counts accepted by cocoon and cocoon-macos CLI size flags.
func NormalizeSizeArg(raw string) string {
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
	if opts.Pull || opts.FromDir != "" {
		args = append(args, "--pull")
	}
	if opts.RestoreMode != "" && opts.Backend != BackendFirecracker {
		args = append(args, "--restore-mode", string(opts.RestoreMode))
	}
	if opts.NICs != nil && opts.Backend != BackendFirecracker {
		args = append(args, "--nics", strconv.Itoa(*opts.NICs))
	}
	args = appendCPUPolicyArgs(args, opts.CPUPolicy)
	if opts.FromDir != "" {
		return append(args, "--from-dir", opts.FromDir)
	}
	return append(args, opts.From)
}

func buildRunArgs(opts RunOptions) []string {
	args := []string{"vm", "run", "--output", "json"}
	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}
	if opts.CPU > 0 {
		args = append(args, "--cpu", strconv.Itoa(opts.CPU))
	}
	args = appendCPUPolicyArgs(args, opts.CPUPolicy)
	if memory := NormalizeSizeArg(opts.Memory); memory != "" {
		args = append(args, "--memory", memory)
	}
	if storage := NormalizeSizeArg(opts.Storage); storage != "" {
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

func appendCPUPolicyArgs(args []string, policy CPUPolicy) []string {
	if policy.CPUWeight > 0 {
		args = append(args, "--cpu-weight", strconv.Itoa(policy.CPUWeight))
	}
	if policy.CPUQuotaUs > 0 {
		args = append(args, "--cpu-quota-us", strconv.FormatInt(policy.CPUQuotaUs, 10))
	}
	if policy.CPUPeriodUs > 0 {
		args = append(args, "--cpu-period-us", strconv.FormatInt(policy.CPUPeriodUs, 10))
	}
	return args
}

// buildExecArgs assembles `cocoon vm exec [-i] [-e KEY=VAL...] <vmID> -- <argv>...`, sorting env keys for a deterministic argv.
func buildExecArgs(vmID string, argv []string, env map[string]string, interactive bool) []string {
	args := make([]string, 0, 5+2*len(env)+len(argv)) //nolint:mnd
	args = append(args, "vm", "exec")
	if interactive {
		args = append(args, "-i")
	}
	for _, k := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(args, vmID, "--")
	args = append(args, argv...)
	return args
}

// parseVMFromStatusJSON decodes a vm status event using the inspect wire format; returns a zero VM on decode failure.
func parseVMFromStatusJSON(data []byte) VM {
	var d inspectJSON
	if json.Unmarshal(data, &d) != nil {
		return VM{}
	}
	return *inspectJSONToVM(d)
}

func cocoonCmdError(op, ref string, err error, output []byte) error {
	return fmt.Errorf("cocoon %s %s: %w (output: %s)", op, ref, err, strings.TrimSpace(string(output)))
}

func cocoonWait(cmd *exec.Cmd, op string) func() error {
	return func() error {
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		return nil
	}
}

func startCmdPipe[P io.Closer](ctx context.Context, cmd *exec.Cmd, pipe func() (P, error), op string) (P, func() error, error) {
	var zero P
	p, err := pipe()
	if err != nil {
		return zero, nil, fmt.Errorf("%s pipe: %w", op, err)
	}
	if err := widenPipe(p); err != nil {
		log.WithFunc("vm.startCmdPipe").Debugf(ctx, "widen %s pipe: %v", op, err)
	}
	if err := cmd.Start(); err != nil {
		_ = p.Close()
		return zero, nil, fmt.Errorf("start %s: %w", op, err)
	}
	return p, cocoonWait(cmd, op), nil
}

// isCocoonNotFound detects cocoon's VM-not-found signal via VM-specific phrases, so an unrelated "not found" stderr isn't promoted to VMGone.
func isCocoonNotFound(err error) bool {
	return errContainsAny(err, "vm not found", "no such vm")
}

func isCocoonSnapshotNotFound(err error) bool {
	return errContainsAny(err, "snapshot not found", "no such snapshot")
}

func errContainsAny(err error, phrases ...string) bool {
	if err == nil {
		return false
	}
	return containsAny(err.Error(), phrases...)
}

func containsAny(s string, phrases ...string) bool {
	lowered := strings.ToLower(s)
	return slices.ContainsFunc(phrases, func(p string) bool { return strings.Contains(lowered, p) })
}

// snapshotNameHolderID pulls the holder's ID out of cocoon's same-name rejection; "" when the message names no holder.
func snapshotNameHolderID(out string) string {
	for _, marker := range []string{"already in use by ", "held by "} {
		_, after, found := strings.Cut(out, marker)
		if !found {
			continue
		}
		fields := strings.Fields(after)
		if len(fields) == 0 {
			continue
		}
		if id := strings.TrimSuffix(fields[0], ")"); id != "" {
			return id
		}
	}
	return ""
}
