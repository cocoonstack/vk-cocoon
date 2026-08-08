package cocoon

// macOS guests (os=macos) are QEMU/KVM VMs dispatched to the standalone
// cocoon-macos binary instead of the cocoon CLI. The lifecycle is deliberately
// self-contained: CreatePod/DeletePod branch here before any cloud-hypervisor
// machinery (adopt-by-name, hibernate evidence, snapshot pull, post-clone)
// runs, because none of it applies to a QEMU guest — cocoon-macos snapshots
// are offline disk snapshots, so hibernate/wake and fork have no macOS
// equivalent. What the path does reuse is generic: the VM tables, the
// runtime-annotation contract, the probes.Manager readiness loop, and the
// lifecycle-state machine.
//
// Networking is tap-only: the guest joins the cocoon-net bridge (cni0 by
// default) and DHCPs a routed IP, resolved through the same lease parser the
// cloud-hypervisor path uses. cocoon-macos' SLIRP mode (--ssh-port
// host-forward) is deliberately never scheduled: vk-cocoon nodes always run
// cocoon-net, and SLIRP accepts TCP before the guest is up, which defeats
// connect-based probing.
//
// A vk-cocoon restart must never relaunch a live guest: two QEMU processes on
// one overlay corrupt the disk. CreatePod replays therefore adopt a live
// record (PID probe) and `vm start` a dead one instead of `vm run`.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"

	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	// defaultMacosBinary is used when Provider.MacosBin is unset.
	defaultMacosBinary = "/usr/local/bin/cocoon-macos"

	// macosHypervisor tags macOS VMs in the shared tables; the CH event and
	// resume paths key off it to skip them.
	macosHypervisor = "qemu"
	macosVMIDPrefix = macosHypervisor + "-"

	macosDefaultCPUs  = 4
	macosDefaultMemMB = 8192

	// macosDefaultProbeSlot feeds macosVNCDisplay when no probe-port slot is
	// annotated: 2222 → display 22 → VNC host port 5922.
	macosDefaultProbeSlot = 2222
	macosVNCPortBase      = 5900

	// macosCNIBridge is the default cocoon-net host bridge a macOS guest joins
	// for a DHCP'd routed IP — the same bridge cloud-hypervisor VMs use.
	// Override per-node with COCOON_MACOS_BRIDGE.
	macosCNIBridge = "cni0"

	macosGuestSSHPort = "22"

	// macosLaunchTimeout bounds a detached `vm run`/`vm start`: the CLI
	// returns once qemu daemonizes, so anything longer is a wedged launch
	// that must not pin a pod worker forever.
	macosLaunchTimeout = 5 * time.Minute

	// macosInspectRetryEvery rate-limits the probe's record backfill so a
	// missing record costs one subprocess per interval, not one per tick.
	macosInspectRetryEvery = 10 * time.Second
)

func isMacosSpec(spec meta.VMSpec) bool {
	return strings.EqualFold(strings.TrimSpace(spec.OS), string(cocoonv1.OSMacos))
}

func macosVMID(vmName string) string { return macosVMIDPrefix + vmName }

func isMacosVM(v *vm.VM) bool { return v != nil && v.Hypervisor == macosHypervisor }

func (p *Provider) macosBridge() string { return cmp.Or(p.MacosBridge, macosCNIBridge) }

// macosVNCDisplay maps the per-node probe-port slot to a VNC display so two
// macOS guests on one node never collide on the VNC host port (5900+display).
// For macOS pods the probe-port annotation is a slot allocation, not a guest
// port to dial — readiness always probes the guest's own sshd on :22. Slot
// allocators hand out per-node-unique ports, so the last two digits are a
// stable, collision-free display in practice.
func macosVNCDisplay(spec meta.VMSpec) int {
	slot := macosDefaultProbeSlot
	if raw := strings.TrimSpace(spec.ProbePort); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 && v <= 65535 {
			slot = v
		}
	}
	d := slot % 100
	if d <= 0 {
		d = 1
	}
	return d
}

func macosCPUs(pod *corev1.Pod) int {
	if cpus, _ := vmResourceOverrides(pod); cpus > 0 {
		return cpus
	}
	return macosDefaultCPUs
}

// macosMemMB returns whole MiB because cocoon-macos' --memory flag is MiB,
// unlike cocoon's byte-normalized size args.
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

// macosVMRecord is the subset of cocoon-macos `vm inspect` JSON needed to
// decide adopt-vs-restart after a vk-cocoon restart and to resolve the
// guest's cocoon-net address.
type macosVMRecord struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	PID   int    `json:"pid"`
	MAC   string `json:"mac"`
	Tap   string `json:"tap"`
}

// applyMacosRecord copies the inspect record's runtime identity onto v.
func applyMacosRecord(v *vm.VM, rec *macosVMRecord) {
	if rec == nil {
		return
	}
	v.PID, v.MAC = rec.PID, rec.MAC
	if rec.Tap != "" {
		v.NetworkConfigs = []*vm.NetworkConfig{{Tap: rec.Tap, MAC: rec.MAC}}
	}
}

// createMacosPod launches (or re-adopts) the QEMU guest for a pod. CreatePod
// has already claimed the key and marked lifecycle-state=creating.
func (p *Provider) createMacosPod(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec) error {
	logger := log.WithFunc("Provider.createMacosPod")
	key := meta.PodKey(pod.Namespace, pod.Name)
	if spec.Image == "" {
		return p.failCreate(ctx, pod, false, "CreateBringUpFailed",
			fmt.Errorf("macos pod %s missing %s annotation", key, meta.AnnotationImage))
	}
	disp := macosVNCDisplay(spec)
	logger.Infof(ctx, "%s: macOS dispatch vm=%s image=%s vnc=%d", key, spec.VMName, spec.Image, macosVNCPortBase+disp)

	if p.macosAlreadyTracked(key, spec.VMName) {
		logger.Infof(ctx, "%s: macOS VM %s already tracked; adopting duplicate CreatePod", key, spec.VMName)
		// Keep the live probe agent (restarting one discards its state for a
		// synchronous re-probe); re-assert Ready because the shared CreatePod
		// prologue just downgraded lifecycle-state to creating.
		if p.Probes == nil || p.Probes.Get(key).LastSeen.IsZero() {
			p.startMacosProbe(pod)
		}
		p.publishMacosReadiness(ctx, pod.Namespace, pod.Name)
		metrics.PodLifecycleTotal.WithLabelValues("create", "ok", "adopted").Inc()
		return nil
	}

	rec := p.macosInspect(ctx, spec.VMName)
	switch {
	case rec != nil && p.macosProcessAlive(rec.PID):
		logger.Infof(ctx, "%s: adopting live macOS VM %s (pid %d)", key, spec.VMName, rec.PID)
	case rec != nil:
		logger.Infof(ctx, "%s: macOS VM %s record exists but process is dead — `vm start`", key, spec.VMName)
		if out, err := p.macosExec(ctx, "vm", "start", spec.VMName); err != nil {
			return p.failCreate(ctx, pod, false, "CreateBringUpFailed",
				fmt.Errorf("cocoon-macos vm start %s: %w: %s", spec.VMName, err, strings.TrimSpace(out)))
		}
		rec = p.macosInspect(ctx, spec.VMName)
	default:
		if err := p.ensureMacosImage(ctx, spec.Image); err != nil {
			return p.failCreate(ctx, pod, false, "CreateBringUpFailed", err)
		}
		args := []string{
			"vm", "run", "--name", spec.VMName,
			"--cpus", strconv.Itoa(macosCPUs(pod)),
			"--memory", strconv.Itoa(macosMemMB(pod)),
			"--vnc", strconv.Itoa(disp),
			"--random-smbios",
			"--net", "tap", "--bridge", p.macosBridge(),
			spec.Image,
		}
		if out, err := p.macosExec(ctx, args...); err != nil {
			// An inspect failure above is indistinguishable from a missing record,
			// so a restart replay can reach this branch while a same-name guest is
			// alive, with `vm run` merely reporting the name collision. Never turn
			// that ambiguous error into `vm rm`.
			return p.failCreate(ctx, pod, false, "CreateBringUpFailed",
				fmt.Errorf("cocoon-macos vm run %s: %w: %s", spec.VMName, err, strings.TrimSpace(out)))
		}
		logger.Infof(ctx, "%s: launched macOS VM %s (bridge=%s, vnc=%d)", key, spec.VMName, p.macosBridge(), macosVNCPortBase+disp)
		rec = p.macosInspect(ctx, spec.VMName)
	}
	p.registerMacosVM(ctx, pod, spec, rec)

	p.mu.Lock()
	pod.Status.Phase = corev1.PodRunning
	now := metav1.Now()
	pod.Status.StartTime = &now
	p.mu.Unlock()
	// Ready is deferred to the SSH probe, like the Windows SAC path defers it:
	// a cold macOS boot takes minutes. The probe's first run happened
	// synchronously in registerMacosVM, and onUpdate fires only on transitions
	// — so a live adoption that is already reachable needs this explicit
	// publication.
	p.publishMacosReadiness(ctx, pod.Namespace, pod.Name)
	metrics.PodLifecycleTotal.WithLabelValues("create", "ok", "").Inc()
	return nil
}

// macosAlreadyTracked reports whether this key already tracks the same-named
// macOS VM, so a CreatePod replay skips a duplicate `vm run`.
func (p *Provider) macosAlreadyTracked(key, vmName string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	v := p.vmsByPod[key]
	return isMacosVM(v) && v.Name == vmName
}

// registerMacosVM records the guest in the shared tables, publishes the
// runtime annotations (VMID/IP/VNC port), and starts the SSH readiness probe.
func (p *Provider) registerMacosVM(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, rec *macosVMRecord) {
	v := &vm.VM{
		ID:         macosVMID(spec.VMName),
		Name:       spec.VMName,
		Hypervisor: macosHypervisor,
		State:      vm.StateRunning,
	}
	applyMacosRecord(v, rec)
	p.trackPod(pod, v)

	rt := meta.VMRuntime{
		VMID:    v.ID,
		IP:      p.resolveVMIP(pod.Namespace, pod.Name, v),
		VNCPort: int32(macosVNCPortBase + macosVNCDisplay(spec)), //nolint:gosec // display is bounded 1..99 by macosVNCDisplay
	}
	vncPort := strconv.Itoa(int(rt.VNCPort))
	p.mu.RLock()
	if rt.IP == "" {
		// Restart adoption before the lease parser sees the guest again: keep
		// the pre-restart address instead of clearing it; the probe republishes
		// the live one on its next Ready flip.
		rt.IP = pod.Annotations[meta.AnnotationIP]
	}
	applied := pod.Annotations[meta.AnnotationVMID] == rt.VMID &&
		pod.Annotations[meta.AnnotationIP] == rt.IP &&
		pod.Annotations[meta.AnnotationVNCPort] == vncPort
	p.mu.RUnlock()
	if !applied {
		p.applyVMRuntime(ctx, pod, rt)
	}
	p.startMacosProbe(pod)
}

func (p *Provider) startMacosProbe(pod *corev1.Pod) {
	p.startProbe(pod,
		p.buildMacosProbe(pod.Namespace, pod.Name),
		p.buildMacosOnUpdate(pod.Namespace, pod.Name))
}

// buildMacosProbe probes the guest's sshd on its cocoon-net IP. The lease
// usually appears minutes before sshd does (OpenCore → kernel → loginwindow →
// sshd on a cold boot), so "no lease yet" and "no banner yet" are distinct
// not-ready messages.
func (p *Provider) buildMacosProbe(namespace, name string) probes.Probe {
	// Goroutine-confined: the probes.Manager runs one probe at a time per agent.
	var lastInspect time.Time
	return func(ctx context.Context) (bool, string) {
		v := p.vmForPod(namespace, name)
		if v == nil {
			return false, "vm gone"
		}
		if v.MAC == "" {
			// Self-heal a record registered while `vm inspect` was transiently
			// failing: without the MAC the guest's lease can never resolve.
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
		ip := p.resolveVMIP(namespace, name, v)
		if ip == "" {
			return false, "waiting for guest dhcp lease"
		}
		return macosSSHReady(ctx, net.JoinHostPort(ip, macosGuestSSHPort))
	}
}

// buildMacosOnUpdate adapts publishMacosReadiness to the probe callback shape.
func (p *Provider) buildMacosOnUpdate(namespace, name string) probes.OnUpdate {
	return func(ctx context.Context) {
		p.publishMacosReadiness(ctx, namespace, name)
	}
}

// publishMacosReadiness flips lifecycle-state=ready once the SSH probe is
// green; the generic buildOnUpdate only refreshes status, which would leave
// the annotation at creating forever.
func (p *Provider) publishMacosReadiness(ctx context.Context, namespace, name string) {
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		log.WithFunc("Provider.publishMacosReadiness").
			Errorf(ctx, err, "pod %s/%s lookup failed, skipping notify", namespace, name)
		return
	}
	ready := p.Probes != nil && p.Probes.Get(meta.PodKey(namespace, name)).Ready
	if !ready || p.lifecycleAlreadyFailed(pod) {
		p.refreshStatus(ctx, pod)
		p.notify(pod)
		return
	}
	// The lease usually resolves after the create-time annotation patch, so
	// re-publish the IP the probe just dialed before flipping Ready.
	if v := p.vmForPod(namespace, name); v != nil && v.IP != "" && pod.Annotations[meta.AnnotationIP] != v.IP {
		p.applyRuntime(ctx, pod, v)
	}
	p.markReadyPublished(ctx, pod)
}

// macosSSHReady dials addr and requires the "SSH-" banner: a bare TCP accept
// is not proof of life while the guest is still booting.
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

// deleteMacosPod tears down the QEMU guest (`vm rm` also terminates the qemu
// process) and drops the pod from the tables. There is no snapshot-on-delete:
// cocoon-macos snapshots are offline, so ShouldSnapshotVM cannot apply.
func (p *Provider) deleteMacosPod(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec) error {
	logger := log.WithFunc("Provider.deleteMacosPod")
	vmName := spec.VMName
	if v := p.vmForPod(pod.Namespace, pod.Name); v != nil && v.Name != "" {
		vmName = v.Name
	}
	if vmName == "" {
		p.forgetPod(pod.Namespace, pod.Name)
		metrics.PodLifecycleTotal.WithLabelValues("delete", "skipped", "no_vm").Inc()
		return nil
	}
	logger.Infof(ctx, "%s/%s: removing macOS VM %s", pod.Namespace, pod.Name, vmName)
	if out, err := p.macosExec(ctx, "vm", "rm", vmName); err != nil && !macosVMMissing(out) {
		metrics.PodLifecycleTotal.WithLabelValues("delete", "failed", "").Inc()
		return fmt.Errorf("cocoon-macos vm rm %s: %w: %s", vmName, err, strings.TrimSpace(out))
	}
	p.forgetPod(pod.Namespace, pod.Name)
	pod.Status.Phase = corev1.PodSucceeded
	p.notify(pod)
	metrics.PodLifecycleTotal.WithLabelValues("delete", "ok", "").Inc()
	return nil
}

// macosVMMissing matches cocoon-macos' missing-VM output. A missing binary
// produces no output at all (the error comes from exec), so it cannot match.
func macosVMMissing(out string) bool {
	s := strings.ToLower(out)
	return strings.Contains(s, "not found") || strings.Contains(s, "no such")
}

// reconcileMacosPod re-adopts a live QEMU guest after a vk-cocoon restart.
// cocoon-macos VMs never appear in Runtime.List, so the generic orphan/adopt
// machinery cannot see them; a dead or missing record is left for the
// CreatePod replay, which restarts it in place.
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
	// Seed before the probe starts (registerMacosVM): its first run may flip
	// Ready, and a later seed would overwrite that intent with the pod's
	// pre-restart annotation.
	p.seedLifecycleIntentFromPod(pod)
	p.registerMacosVM(ctx, pod, spec, rec)
}

// ensureMacosImage materializes the image in the node-local cocoon-macos
// store, deduping concurrent pulls of the same ref.
func (p *Provider) ensureMacosImage(ctx context.Context, image string) error {
	ch := p.macosImageSF.DoChan(image, func() (any, error) {
		// Detached like the other import flights: one caller's abort must not
		// fail the flight for its remaining waiters, and the shared timeout
		// backstops a wedged pull.
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

// macosInspect returns the cocoon-macos record for a VM, or nil if it has none.
func (p *Provider) macosInspect(ctx context.Context, vmName string) *macosVMRecord {
	out, err := p.macosExec(ctx, "vm", "inspect", vmName)
	if err != nil {
		return nil
	}
	var rec macosVMRecord
	if json.Unmarshal([]byte(out), &rec) != nil || strings.TrimSpace(rec.Name) == "" {
		return nil
	}
	return &rec
}

// macosExec runs a cocoon-macos subcommand. `vm run`/`vm start` detach from
// the caller's cancellation (qemu daemonizes itself, but the CLI must survive
// an aborted CreatePod or a half-launched guest leaks outside its record)
// under a launch timeout of their own; everything else stays cancellable.
func (p *Provider) macosExec(ctx context.Context, args ...string) (string, error) {
	if p.macosExecFn != nil {
		return p.macosExecFn(ctx, args...)
	}
	if len(args) >= 2 && args[0] == "vm" && (args[1] == "run" || args[1] == "start") {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), macosLaunchTimeout)
		defer cancel()
	}
	bin := cmp.Or(p.MacosBin, defaultMacosBinary)
	log.WithFunc("Provider.macosExec").Debugf(ctx, "%s %s", bin, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // path comes from operator config, not untrusted input
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}

// macosProcessAlive reports whether pid names a live process this node can signal.
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
