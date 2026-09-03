package cocoon

// os=macos pods dispatch to the cocoon-macos binary; a replay adopts or starts the existing record, never runs a second QEMU on the disk.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	defaultMacosBinary  = "/usr/local/bin/cocoon-macos"
	maxVNCPasswordBytes = 8

	// macosHypervisor tags macOS VMs in the shared tables; the CH event and resume paths key off it to skip them.
	macosHypervisor = "qemu"
	macosVMIDPrefix = macosHypervisor + "-"

	macosDefaultCPUs  = 4
	macosDefaultMemMB = 8192

	macosVNCPortBase = 5900
	macosVNCPortSpan = 100

	macosGuestSSHPort = "22"

	// macosLaunchTimeout bounds a detached `vm run`/`vm start`: the CLI returns once qemu daemonizes.
	macosLaunchTimeout       = 5 * time.Minute
	macosCommandCleanupGrace = 90 * time.Second

	// macosInspectRetryEvery rate-limits the probe's record backfill to one subprocess per interval, not one per tick.
	macosInspectRetryEvery = 10 * time.Second
)

// claimMacosVNCPort reserves a node-unique VNC port for key, preferring a previously published one; 0 means exhausted.
func (p *Provider) claimMacosVNCPort(key string, preferred int) int {
	if p.MacosVNCPassword == "" {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if port, ok := p.macosVNC[key]; ok {
		return port
	}
	used := make(map[int]bool, len(p.macosVNC))
	for _, port := range p.macosVNC {
		used[port] = true
	}
	if preferred >= macosVNCPortBase && preferred < macosVNCPortBase+macosVNCPortSpan && !used[preferred] {
		p.macosVNC[key] = preferred
		return preferred
	}
	for d := range macosVNCPortSpan {
		if port := macosVNCPortBase + d; !used[port] {
			p.macosVNC[key] = port
			return port
		}
	}
	return 0
}

// adoptMacosVNCPort records the display a live guest actually serves.
func (p *Provider) adoptMacosVNCPort(key string, rec *macosVMRecord) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rec.VNC < 0 {
		delete(p.macosVNC, key)
		return 0
	}
	port := macosVNCPortBase + rec.VNC
	p.macosVNC[key] = port
	return port
}

func (p *Provider) macosVNCPortFor(key string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.macosVNC[key]
}

// createMacosPod launches (or re-adopts) the QEMU guest; CreatePod already claimed the key and marked it creating.
func (p *Provider) createMacosPod(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec) error {
	logger := log.WithFunc("Provider.createMacosPod")
	key := meta.PodKey(pod.Namespace, pod.Name)
	if spec.Image == "" {
		return p.failCreate(ctx, pod, false, "CreateBringUpFailed",
			fmt.Errorf("macos pod %s missing %s annotation", key, meta.AnnotationImage))
	}
	logger.Infof(ctx, "%s: macOS dispatch vm=%s image=%s", key, spec.VMName, spec.Image)

	if p.macosAlreadyTracked(key, spec.VMName) {
		logger.Infof(ctx, "%s: macOS VM %s already tracked; adopting duplicate CreatePod", key, spec.VMName)
		// keep a live probe agent (a restart discards its state); re-assert Ready since the CreatePod prologue downgraded it.
		if p.Probes == nil || p.Probes.Get(key).LastSeen.IsZero() {
			p.startMacosProbe(pod)
		}
		p.publishMacosReadiness(ctx, pod.Namespace, pod.Name)
		metrics.PodLifecycleTotal.WithLabelValues("create", "ok", "adopted").Inc()
		return nil
	}

	preferred := int(meta.ParseVMRuntime(pod).VNCPort)
	var port int
	rec := p.macosInspect(ctx, spec.VMName)
	switch {
	case rec != nil && p.macosProcessAlive(rec.PID):
		port = p.adoptMacosVNCPort(key, rec)
		logger.Infof(ctx, "%s: adopting live macOS VM %s (pid %d)", key, spec.VMName, rec.PID)
	case rec != nil:
		port = p.claimMacosVNCPort(key, preferred)
		logger.Infof(ctx, "%s: macOS VM %s record exists but process is dead — `vm start`", key, spec.VMName)
		if out, err := p.startMacosVM(ctx, spec.VMName, port); err != nil {
			return p.failCreate(ctx, pod, false, "CreateBringUpFailed",
				fmt.Errorf("cocoon-macos vm start %s: %w: %s", spec.VMName, err, strings.TrimSpace(out)))
		}
		rec = p.macosInspect(ctx, spec.VMName)
	default:
		port = p.claimMacosVNCPort(key, preferred)
		if err := p.ensureMacosImage(ctx, spec.Image); err != nil {
			return p.failCreate(ctx, pod, false, "CreateBringUpFailed", err)
		}
		args, err := p.appendMacosVNCArgs([]string{
			"vm", "run", "--name", spec.VMName,
			"--cpus", strconv.Itoa(macosCPUs(pod)),
			"--memory", strconv.Itoa(macosMemMB(pod)),
		}, port)
		if err != nil {
			return p.failCreate(ctx, pod, false, "CreateBringUpFailed", err)
		}
		if storage := vm.NormalizeSizeArg(spec.Storage); storage != "" {
			args = append(args, "--storage", storage)
		}
		args = append(args, "--exit-on-reboot", "--random-smbios", "--net", "cni", spec.Image)
		if out, err := p.macosExec(ctx, args...); err != nil {
			// a failed inspect is indistinguishable from a missing record, so never turn this ambiguous error into `vm rm`.
			return p.failCreate(ctx, pod, false, "CreateBringUpFailed",
				fmt.Errorf("cocoon-macos vm run %s: %w: %s", spec.VMName, err, strings.TrimSpace(out)))
		}
		logger.Infof(ctx, "%s: launched macOS VM %s (net=cni, vnc=%d)", key, spec.VMName, port)
		rec = p.macosInspect(ctx, spec.VMName)
	}
	if !p.registerMacosVM(ctx, pod, spec, rec, port) {
		p.skipSuperseded(ctx, pod, "create")
		return nil
	}

	p.mu.Lock()
	pod.Status.Phase = corev1.PodRunning
	now := metav1.Now()
	pod.Status.StartTime = &now
	p.mu.Unlock()
	// ready defers to the probe; onUpdate only fires on transitions, so a reachable adoption needs this explicit publish.
	p.publishMacosReadiness(ctx, pod.Namespace, pod.Name)
	metrics.PodLifecycleTotal.WithLabelValues("create", "ok", "").Inc()
	return nil
}

func (p *Provider) macosAlreadyTracked(key, vmName string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	v := p.vmsByPod[key]
	return isMacosVM(v) && v.Name == vmName
}

// registerMacosVM tracks the guest, publishes VMID/IP/VNC annotations, and starts the macOS readiness probe.
func (p *Provider) registerMacosVM(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, rec *macosVMRecord, vncPort int) bool {
	v := &vm.VM{
		ID:         macosVMID(spec.VMName),
		Name:       spec.VMName,
		Hypervisor: macosHypervisor,
		State:      vm.StateRunning,
	}
	applyMacosRecord(v, rec)
	p.seedLeaseIP(v)

	rt := meta.VMRuntime{
		VMID:    v.ID,
		IP:      v.IP,
		VNCPort: int32(vncPort), //nolint:gosec // bounded by macosVNCPortBase+macosVNCPortSpan
	}
	if rt.IP == "" {
		// no lease yet (restart adoption): keep the pre-restart address until the probe or status reconciler resolves it.
		p.mu.RLock()
		rt.IP = pod.Annotations[meta.AnnotationIP]
		p.mu.RUnlock()
	}
	if !p.bindRuntime(ctx, pod, v, rt) {
		return false
	}
	if current, err := p.GetPod(ctx, pod.Namespace, pod.Name); err == nil {
		p.reconcileRuntimeEndpoints(ctx, current, rt.IP)
	}
	p.startMacosProbe(pod)
	return true
}

func (p *Provider) startMacosProbe(pod *corev1.Pod) {
	p.startProbe(pod,
		p.buildMacosProbe(pod.Namespace, pod.Name, pod.UID),
		p.buildMacosOnUpdate(pod.Namespace, pod.Name))
}

// buildMacosProbe dials probePort when declared, else sshd: the lease lands minutes before sshd on a cold boot.
func (p *Provider) buildMacosProbe(namespace, name string, uid types.UID) probes.Probe {
	// Goroutine-confined: the probes.Manager runs one probe at a time per agent.
	var lastInspect, lastRestart time.Time
	key := meta.PodKey(namespace, name)
	return func(ctx context.Context) (bool, string) {
		if !p.trackedPodMatches(key, uid) {
			return false, "pod incarnation replaced"
		}
		v := p.vmForPod(namespace, name)
		if v == nil {
			return false, "vm gone"
		}
		if v.MAC == "" {
			// self-heal a record registered while `vm inspect` was failing: without the MAC the guest's lease can never resolve.
			if time.Since(lastInspect) >= macosInspectRetryEvery {
				lastInspect = time.Now()
				if rec := p.macosInspect(ctx, v.Name); rec != nil && rec.MAC != "" {
					v = p.updateTrackedVM(namespace, name, v.ID, func(u *vm.VM) { applyMacosRecord(u, rec) })
				}
			}
			if v == nil || v.MAC == "" {
				return false, "waiting for vm record"
			}
		}
		if v.PID > 0 && !p.macosProcessAlive(v.PID) {
			// the CH event stream and orphan scan cannot see QEMU guests, so a crash is repaired here or never.
			if time.Since(lastRestart) < macosInspectRetryEvery {
				return false, "qemu process dead"
			}
			lastRestart = time.Now()
			return false, p.recoverMacosVM(ctx, namespace, name, uid, v)
		}
		ip := p.resolveVMIP(namespace, name, v)
		if ip == "" {
			return false, "waiting for guest dhcp lease"
		}
		if port := p.probePort(namespace, name); port != "" {
			return p.probeTCP(ctx, ip, port)
		}
		return macosSSHReady(ctx, net.JoinHostPort(ip, macosGuestSSHPort))
	}
}

// recoverMacosVM adopts a live record or `vm start`s a dead one, detached from the probe deadline to avoid a stale PID.
func (p *Provider) recoverMacosVM(ctx context.Context, namespace, name string, uid types.UID, v *vm.VM) string {
	ctx, cancel := detachedLaunchCtx(ctx)
	defer cancel()
	if rec := p.macosInspect(ctx, v.Name); rec != nil && p.macosProcessAlive(rec.PID) {
		p.updateRecoveredMacosVM(ctx, namespace, name, uid, v.ID, rec)
		return "adopted running qemu"
	}
	if out, err := p.startMacosVM(ctx, v.Name, p.macosVNCPortFor(meta.PodKey(namespace, name))); err != nil {
		return "qemu restart: " + strings.TrimSpace(out) + ": " + err.Error()
	}
	if rec := p.macosInspect(ctx, v.Name); rec != nil {
		p.updateRecoveredMacosVM(ctx, namespace, name, uid, v.ID, rec)
	}
	return "qemu restarted"
}

func (p *Provider) updateRecoveredMacosVM(ctx context.Context, namespace, name string, uid types.UID, vmID string, rec *macosVMRecord) {
	key := meta.PodKey(namespace, name)
	if !p.trackedPodMatches(key, uid) {
		return
	}
	v := p.updateTrackedVM(namespace, name, vmID, func(u *vm.VM) { applyMacosRecord(u, rec) })
	if v == nil {
		return
	}
	p.adoptMacosVNCPort(key, rec)
	if pod, err := p.GetPod(ctx, namespace, name); err == nil {
		p.reconcileRuntimeEndpoints(ctx, pod, v.IP)
	}
}

func (p *Provider) buildMacosOnUpdate(namespace, name string) probes.OnUpdate {
	return func(ctx context.Context) {
		p.publishMacosReadiness(ctx, namespace, name)
	}
}

// publishMacosReadiness flips lifecycle-state=ready on a green probe; buildOnUpdate alone only refreshes status.
func (p *Provider) publishMacosReadiness(ctx context.Context, namespace, name string) {
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		log.WithFunc("Provider.publishMacosReadiness").
			Errorf(ctx, err, "pod %s/%s lookup failed, skipping notify", namespace, name)
		return
	}
	ready := p.Probes != nil && p.Probes.Get(meta.PodKey(namespace, name)).Ready
	if !ready || p.lifecycleAlreadyFailed(pod) {
		p.refreshAndNotify(ctx, pod)
		return
	}
	// reconcile runtime endpoints before flipping Ready; the lease usually resolves post-patch.
	if v := p.vmForPod(namespace, name); v != nil && !p.reconcileRuntimeEndpoints(ctx, pod, v.IP) {
		return
	}
	p.markReadyPublished(ctx, pod)
}

// deleteMacosPod tears down the QEMU guest (`vm rm` also terminates the process); snapshot-on-delete cannot apply.
func (p *Provider) deleteMacosPod(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec) error {
	logger := log.WithFunc("Provider.deleteMacosPod")
	vmName := spec.VMName
	v := p.vmForPod(pod.Namespace, pod.Name)
	if v != nil && v.Name != "" {
		vmName = v.Name
	}
	if vmName == "" {
		p.forgetPod(pod.Namespace, pod.Name)
		metrics.PodLifecycleTotal.WithLabelValues("delete", "skipped", "no_vm").Inc()
		return nil
	}
	if v == nil {
		v = &vm.VM{ID: macosVMID(vmName)}
		applyMacosRecord(v, p.macosInspect(ctx, vmName))
	}
	if p.Probes != nil {
		p.Probes.Forget(meta.PodKey(pod.Namespace, pod.Name))
	}
	logger.Infof(ctx, "%s/%s: removing macOS VM %s", pod.Namespace, pod.Name, vmName)
	if out, err := p.macosExec(ctx, "vm", "rm", vmName); err != nil && !macosVMMissing(out) {
		metrics.PodLifecycleTotal.WithLabelValues("delete", "failed", "").Inc()
		return fmt.Errorf("cocoon-macos vm rm %s: %w: %s", vmName, err, strings.TrimSpace(out))
	}
	p.releaseDHCPLeases(ctx, v)
	p.forgetPod(pod.Namespace, pod.Name)
	pod.Status.Phase = corev1.PodSucceeded
	p.notify(pod)
	metrics.PodLifecycleTotal.WithLabelValues("delete", "ok", "").Inc()
	return nil
}

// reconcileMacosPod re-adopts a live QEMU guest after a restart (invisible to Runtime.List); CreatePod handles a dead record.
func (p *Provider) reconcileMacosPod(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec) {
	logger := log.WithFunc("Provider.reconcileMacosPod")
	if spec.VMName == "" {
		return
	}
	rec := p.macosInspect(ctx, spec.VMName)
	if rec == nil || !p.macosProcessAlive(rec.PID) {
		logger.Infof(ctx, "pod %s/%s: no live macOS VM %s; CreatePod will restart it",
			pod.Namespace, pod.Name, spec.VMName)
		return
	}
	logger.Infof(ctx, "adopting live macOS VM %s (pid %d) for pod %s/%s",
		spec.VMName, rec.PID, pod.Namespace, pod.Name)
	// seed before the probe's first run: it may flip Ready, and a later seed would overwrite that.
	p.seedLifecycleIntentFromPod(pod)
	if !p.registerMacosVM(ctx, pod, spec, rec, p.adoptMacosVNCPort(meta.PodKey(pod.Namespace, pod.Name), rec)) {
		p.skipSuperseded(ctx, pod, "create")
		return
	}
	p.publishMacosReadiness(ctx, pod.Namespace, pod.Name)
}

// ensureMacosImage materializes the image in the node-local cocoon-macos store.
func (p *Provider) ensureMacosImage(ctx context.Context, image string) error {
	ch := p.macosImageSF.DoChan(image, func() (any, error) {
		// detached like the other import flights: one caller's abort must not fail the flight for its remaining waiters.
		shared, cancel := p.detachedImportContext()
		defer cancel()
		if p.macosImagePresent(shared, image) {
			return image, nil
		}
		log.WithFunc("Provider.ensureMacosImage").Infof(ctx, "macOS image %s not in local store — pulling", image)
		if out, err := p.macosExec(shared, "image", "pull", image); err != nil {
			return "", fmt.Errorf("macos image %q not materialized and pull failed: %w: %s", image, err, strings.TrimSpace(out))
		}
		if !p.macosImagePresent(shared, image) {
			return "", fmt.Errorf("macos image %q is absent after pull", image)
		}
		return image, nil
	})
	_, err := awaitFlight(ctx, ch, "")
	return err
}

func (p *Provider) macosImagePresent(ctx context.Context, image string) bool {
	_, err := p.macosExec(ctx, "image", "inspect", image)
	return err == nil
}

// startMacosVM boots a dead record, re-asserting the VNC display (a bare `vm start` disables it).
func (p *Provider) startMacosVM(ctx context.Context, vmName string, port int) (string, error) {
	args, err := p.appendMacosVNCArgs([]string{"vm", "start"}, port)
	if err != nil {
		return "", err
	}
	return p.macosExec(ctx, append(args, vmName)...)
}

// macosExec runs a cocoon-macos subcommand; `vm run`/`vm start` detach so an abort can't kill it mid-launch.
func (p *Provider) macosExec(ctx context.Context, args ...string) (string, error) {
	if p.macosExecFn != nil {
		return p.macosExecFn(ctx, args...)
	}
	isLaunch := len(args) >= 2 && args[0] == "vm" && (args[1] == "run" || args[1] == "start")
	isLifecycleMutation := isLaunch || (len(args) >= 2 && args[0] == "vm" && args[1] == "rm")
	if isLaunch {
		var cancel context.CancelFunc
		ctx, cancel = detachedLaunchCtx(ctx)
		defer cancel()
	}
	bin := cmp.Or(p.MacosBin, defaultMacosBinary)
	log.WithFunc("Provider.macosExec").Debugf(ctx, "%s %s", bin, formatMacosArgsForLog(args))
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // path comes from operator config, not untrusted input
	if isLifecycleMutation {
		configureMacosLifecycleCommand(cmd)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}

func (p *Provider) appendMacosVNCArgs(args []string, port int) ([]string, error) {
	if port == 0 || p.MacosVNCPassword == "" {
		return args, nil
	}
	if err := ValidateMacosVNCPassword(p.MacosVNCPassword); err != nil {
		return nil, fmt.Errorf("COCOON_MACOS_VNC_PASSWORD %w", err)
	}
	// argv exposure is acceptable because nodes have no unprivileged local users and guests cannot read host /proc.
	return append(args, "--vnc", strconv.Itoa(port-macosVNCPortBase), "--vnc-password", p.MacosVNCPassword), nil
}

func (p *Provider) macosProcessAlive(pid int) bool {
	if p.macosProcessAliveFn != nil {
		return p.macosProcessAliveFn(pid)
	}
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// macosVMRecord is the subset of cocoon-macos `vm inspect` JSON needed for adopt-vs-restart and lease resolution.
type macosVMRecord struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
	MAC  string `json:"mac"`
	Tap  string `json:"tap"`
	Disk string `json:"disk"`
	VNC  int    `json:"vnc"` // display number; -1 = off
}

func (p *Provider) macosInspect(ctx context.Context, vmName string) *macosVMRecord {
	out, err := p.macosExec(ctx, "vm", "inspect", vmName)
	if err != nil {
		return nil
	}
	rec := macosVMRecord{VNC: -1}
	if json.Unmarshal([]byte(out), &rec) != nil || strings.TrimSpace(rec.Name) == "" {
		return nil
	}
	return &rec
}

// ValidateMacosVNCPassword rejects a set password QEMU cannot accept; empty means VNC off.
func ValidateMacosVNCPassword(pw string) error {
	if pw == "" {
		return nil
	}
	if len(pw) > maxVNCPasswordBytes || strings.ContainsFunc(pw, unicode.IsControl) {
		return fmt.Errorf("must be at most %d bytes with no control characters", maxVNCPasswordBytes)
	}
	return nil
}

func applyMacosRecord(v *vm.VM, rec *macosVMRecord) {
	if rec == nil {
		return
	}
	v.PID, v.MAC, v.DiskPath = rec.PID, rec.MAC, rec.Disk
	if rec.Tap != "" {
		v.NetworkConfigs = []*vm.NetworkConfig{{Tap: rec.Tap, MAC: rec.MAC}}
	}
}

func isMacosSpec(spec meta.VMSpec) bool {
	return strings.EqualFold(strings.TrimSpace(spec.OS), string(cocoonv1.OSMacos))
}

func macosVMID(vmName string) string { return macosVMIDPrefix + vmName }

func isMacosVM(v *vm.VM) bool { return v != nil && v.Hypervisor == macosHypervisor }

func macosCPUs(pod *corev1.Pod) int {
	cpus := macosDefaultCPUs
	if o, _ := vmResourceOverrides(pod); o > 0 {
		cpus = o
	}
	return cpus + cpus%2
}

// macosMemMB returns whole MiB because cocoon-macos' --memory flag is MiB, unlike cocoon's byte-normalized size args.
func macosMemMB(pod *corev1.Pod) int {
	if len(pod.Spec.Containers) > 0 {
		resources := pod.Spec.Containers[0].Resources
		q := selectQuantity(resources.Requests, resources.Limits, corev1.ResourceMemory)
		if mb := q.Value() / (1 << 20); mb > 0 {
			return int(mb)
		}
	}
	return macosDefaultMemMB
}

// configureMacosLifecycleCommand swaps CommandContext's SIGKILL for SIGTERM + a bounded wait: cocoon-macos's ctx-aware teardown settles and releases its locks instead of dying mid-write.
func configureMacosLifecycleCommand(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		return nil
	}
	cmd.WaitDelay = macosCommandCleanupGrace
}

func formatMacosArgsForLog(args []string) string {
	redacted := slices.Clone(args)
	for i := 0; i+1 < len(redacted); i++ {
		if redacted[i] == "--vnc-password" {
			redacted[i+1] = "<redacted>"
			i++
		}
	}
	return strings.Join(redacted, " ")
}

func detachedLaunchCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), macosLaunchTimeout)
}

// macosSSHReady dials addr and requires the "SSH-" banner: a bare TCP accept is not proof of life.
func macosSSHReady(ctx context.Context, addr string) (bool, string) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, "ssh probe " + addr + ": " + err.Error()
	}
	defer conn.Close() //nolint:errcheck // probe-only connection
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	buf := make([]byte, 16)
	n, _ := conn.Read(buf)
	if n > 0 && strings.HasPrefix(string(buf[:n]), "SSH-") {
		return true, "ssh ok"
	}
	return false, "no ssh banner at " + addr
}

// macosVMMissing matches cocoon-macos' missing-VM output; a missing binary produces no output, so it cannot match.
func macosVMMissing(out string) bool {
	s := strings.ToLower(out)
	return strings.Contains(s, "not found") || strings.Contains(s, "no such")
}
