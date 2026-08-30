package cocoon

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/manifest"
	"github.com/cocoonstack/cocoon-common/meta"
	commonsnapshot "github.com/cocoonstack/cocoon-common/snapshot"

	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/snapshots"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	// hibernateImportSuffix avoids name collision with the live VM the Clone produces.
	hibernateImportSuffix = "-hibernate-import"

	// defaultWakeFreshIPBudget bounds waitForFreshIP; the budget is generous.
	defaultWakeFreshIPBudget   = 45 * time.Second
	defaultWakeFreshIPInterval = 200 * time.Millisecond

	// defaultWakeRenewNudgeDelay leaves natural DHCP the first 30s before the one-shot renew nudge.
	defaultWakeRenewNudgeDelay = 30 * time.Second

	// guestIpconfigTimeout bounds the vsock exec so a sick guest cannot stall hibernate or wake.
	guestIpconfigTimeout     = 20 * time.Second
	hibernateRollbackTimeout = 30 * time.Second
)

var errStaleLocalSnapshot = errors.New("local snapshot does not match registry tag")

// UpdatePod applies a pod spec update; non-hibernate changes stay no-ops since patching re-enters UpdatePod.
func (p *Provider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("Provider.UpdatePod")
	logger.Infof(ctx, "update pod %s/%s", pod.Namespace, pod.Name)

	if err := p.assertPodSnapshotCompatibility(pod); err != nil {
		metrics.PodLifecycleTotal.WithLabelValues("update", "failed", "snapshot_cpu_class_mismatch").Inc()
		return err
	}

	// Before the lookup, so a post-check read can never see mid-resume state.
	if err := p.backoffIfResuming(pod.Namespace, pod.Name); err != nil {
		return err
	}
	v := p.vmForPod(pod.Namespace, pod.Name)
	// Pod only: re-asserting v could resurrect a row a resume just dropped.
	p.trackPod(pod, nil)

	// os=macos: hibernate cannot apply (offline disk snapshots); every other update is an annotation echo.
	if spec := meta.ParseVMSpec(pod); isMacosSpec(spec) {
		if meta.ReadHibernateState(pod) {
			err := fmt.Errorf("macOS guest %s does not support hibernate", spec.VMName)
			p.failOp(ctx, pod, "HibernateUnsupported", "update", err)
			return err
		}
		return p.noopUpdate(ctx, pod)
	}

	wantHibernate := bool(meta.ReadHibernateState(pod))
	haveVM := v != nil

	switch {
	case wantHibernate && haveVM:
		if err := p.hibernate(ctx, pod, v); err != nil {
			return err
		}
		metrics.PodLifecycleTotal.WithLabelValues("update", "ok", "").Inc()
	case !wantHibernate && !haveVM:
		if err := p.wake(ctx, pod); err != nil {
			return err
		}
		metrics.PodLifecycleTotal.WithLabelValues("update", "ok", "").Inc()
	default:
		return p.noopUpdate(ctx, pod)
	}
	p.refreshAndNotify(ctx, pod)
	return nil
}

// noopUpdate republishes lifecycle intent, skipping refresh+notify so the incoming pod is not echoed back.
func (p *Provider) noopUpdate(ctx context.Context, pod *corev1.Pod) error {
	p.republishLifecycleOnGenerationBump(ctx, pod)
	metrics.PodLifecycleTotal.WithLabelValues("update", "skipped", "noop").Inc()
	return nil
}

// hibernate runs Save -> Push -> Remove; CH+Windows drops the NIC first to dodge a Windows PnP MAC-swap.
func (p *Provider) hibernate(ctx context.Context, pod *corev1.Pod, v *vm.VM) error {
	logger := log.WithFunc("Provider.hibernate")
	spec := meta.ParseVMSpec(pod)
	p.markLifecycleState(ctx, pod, meta.LifecycleStateHibernating, "")
	dropNIC := shouldDropNICBeforeHibernate(spec)
	if dropNIC {
		if err := p.dropNICForHibernate(ctx, v); err != nil {
			p.failOp(ctx, pod, "HibernateNetResizeFailed", "update", err)
			return err
		}
	}
	saveStart := time.Now()
	if err := p.Runtime.SnapshotSave(ctx, v.Name, v.ID); err != nil {
		metrics.SnapshotSaveTotal.WithLabelValues("failed").Inc()
		metrics.HibernateTotal.WithLabelValues("snapshot", "failed").Inc()
		p.rollbackHibernateNIC(ctx, v, dropNIC)
		err = fmt.Errorf("save snapshot %s: %w", v.Name, err)
		p.failOp(ctx, pod, "HibernateSnapshotFailed", "update", err)
		return err
	}
	metrics.SnapshotSaveDuration.Observe(time.Since(saveStart).Seconds())
	metrics.SnapshotSaveTotal.WithLabelValues("ok").Inc()
	metrics.HibernateTotal.WithLabelValues("snapshot", "ok").Inc()
	if p.Pusher != nil {
		pushStart := time.Now()
		// spec.Image rides as the baseimage annotation for hibernateEvidence's identity guard.
		if _, err := p.Pusher.PushSnapshot(ctx, v.Name, v.Name, meta.HibernateSnapshotTag, spec.Image); err != nil {
			metrics.SnapshotPushTotal.WithLabelValues("failed").Inc()
			metrics.HibernateTotal.WithLabelValues("push", "failed").Inc()
			p.rollbackHibernateNIC(ctx, v, dropNIC)
			err = fmt.Errorf("push hibernation snapshot %s: %w", v.Name, err)
			p.failOp(ctx, pod, "HibernatePushFailed", "update", err)
			return err
		}
		metrics.SnapshotPushDuration.Observe(time.Since(pushStart).Seconds())
		metrics.SnapshotPushTotal.WithLabelValues("ok").Inc()
		metrics.HibernateTotal.WithLabelValues("push", "ok").Inc()
	}
	preCleared := true
	if err := p.clearRuntimeAnnotations(ctx, pod); err != nil {
		preCleared = false
		logger.Errorf(ctx, err, "clear pre-remove annotations %s/%s", pod.Namespace, pod.Name)
	}
	if err := p.removeVM(ctx, v); err != nil {
		metrics.HibernateTotal.WithLabelValues("remove", "failed").Inc()
		if p.Registry != nil {
			if delErr := p.Registry.DeleteManifest(ctx, v.Name, meta.HibernateSnapshotTag); delErr != nil {
				logger.Errorf(ctx, delErr, "rollback hibernate push after remove failed for %s", v.Name)
			}
		}
		// VM is still live; restore NIC + VMID/IP so the pod can retry hibernate.
		p.rollbackHibernateNIC(ctx, v, dropNIC)
		p.applyRuntime(ctx, pod, v)
		err = fmt.Errorf("remove vm %s: %w", v.ID, err)
		p.failOp(ctx, pod, "HibernateRemoveFailed", "update", err)
		return err
	}
	metrics.HibernateTotal.WithLabelValues("remove", "ok").Inc()
	if !preCleared {
		// VM is gone; reconcileStaleHibernate is the last fallback if this also fails.
		if err := p.clearRuntimeAnnotations(ctx, pod); err != nil {
			logger.Errorf(ctx, err, "clear hibernate annotations %s/%s (VM already removed)", pod.Namespace, pod.Name)
		}
	}
	p.forgetVMOnly(pod.Namespace, pod.Name)
	p.markLifecycleState(ctx, pod, meta.LifecycleStateHibernated, "")
	if p.Pusher != nil {
		p.emitNormalf(pod, "Hibernated", "snapshot pushed to registry")
	} else {
		p.emitNormalf(pod, "Hibernated", "snapshot saved locally (no registry pusher configured)")
	}
	return nil
}

// dropNICForHibernate releases the lease and detaches the NIC so restored clones DISCOVER, not NAK; best-effort.
func (p *Provider) dropNICForHibernate(ctx context.Context, v *vm.VM) error {
	logger := log.WithFunc("Provider.dropNICForHibernate")
	if err := p.execGuestIpconfig(ctx, v.ID, "release"); err != nil {
		metrics.HibernateTotal.WithLabelValues("dhcp_release", "failed").Inc()
		logger.Warnf(ctx, "dhcp release before hibernate %s: %v (proceeding)", v.Name, err)
	} else {
		metrics.HibernateTotal.WithLabelValues("dhcp_release", "ok").Inc()
	}
	if err := p.Runtime.NetResize(ctx, v.ID, 0); err != nil {
		metrics.HibernateTotal.WithLabelValues("netresize", "failed").Inc()
		// renew regardless, detached from ctx: an exec error doesn't prove the guest skipped the release.
		if renewErr := p.execGuestIpconfig(context.WithoutCancel(ctx), v.ID, "renew"); renewErr != nil {
			logger.Warnf(ctx, "dhcp renew after failed NIC drop %s: %v", v.Name, renewErr)
		}
		return fmt.Errorf("drop NIC pre-hibernate %s: %w", v.Name, err)
	}
	metrics.HibernateTotal.WithLabelValues("netresize", "ok").Inc()
	return nil
}

func (p *Provider) rollbackHibernateNIC(ctx context.Context, v *vm.VM, dropped bool) {
	if !dropped {
		return
	}
	logger := log.WithFunc("Provider.rollbackHibernateNIC")
	// cancel-detached: the triggering failure may be ctx dying, and an online VM must not be left NIC-less.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hibernateRollbackTimeout)
	defer cancel()
	if err := p.Runtime.NetResize(ctx, v.ID, 1); err != nil {
		logger.Errorf(ctx, err, "re-add NIC after hibernate failure %s", v.Name)
		return
	}
	// The pre-hibernate release left the guest unbound; nudge it to re-acquire.
	if err := p.execGuestIpconfig(ctx, v.ID, "renew"); err != nil {
		logger.Warnf(ctx, "dhcp renew after hibernate rollback %s: %v", v.Name, err)
	}
}

// wake restores the VM from the hibernation snapshot; dispatchHibernateRestore handles the post-restore step.
func (p *Provider) wake(ctx context.Context, pod *corev1.Pod) error {
	spec := meta.ParseVMSpec(pod)
	if spec.VMName == "" {
		return nil
	}
	p.markLifecycleState(ctx, pod, meta.LifecycleStateCreating, "")
	// annotations that survived a failed clear at hibernate must not describe the incarnation this wake creates.
	if err := p.clearRuntimeAnnotations(ctx, pod); err != nil {
		log.WithFunc("Provider.wake").Errorf(ctx, err, "clear stale annotations %s/%s", pod.Namespace, pod.Name)
	}
	src, err := p.resolveWakeSource(ctx, spec.VMName)
	if err != nil {
		metrics.WakeTotal.WithLabelValues("failed").Inc()
		p.failOp(ctx, pod, "WakePullFailed", "update", err)
		return err
	}
	cloneStart := time.Now()
	v, err := p.cloneFromHibernate(ctx, spec, src, podCPUPolicy(pod))
	if err != nil {
		metrics.WakeTotal.WithLabelValues("failed").Inc()
		p.failOp(ctx, pod, "WakeCloneFailed", "update", err)
		return err
	}
	metrics.VMBootDuration.WithLabelValues("clone", spec.Backend).Observe(time.Since(cloneStart).Seconds())
	p.applyRuntime(ctx, pod, v)
	p.trackPod(pod, v)
	p.startProbeIfEnabled(pod)
	p.dispatchHibernateRestore(pod, spec, v, "update")
	p.emitNormalf(pod, "Woken", "cloned from %s", src.label())
	return nil
}

// cloneFromHibernate clones from a resolved wake source; CH+Windows snapshots are NIC-less, so it hot-adds a fresh NIC.
func (p *Provider) cloneFromHibernate(ctx context.Context, spec meta.VMSpec, src wakeSource, policy vm.CPUPolicy) (*vm.VM, error) {
	defer src.release()
	if err := p.ensureSnapshotBaseImage(ctx, src.snapshot); err != nil {
		return nil, err
	}
	opts := vm.CloneOptions{
		From:        src.localName,
		FromDir:     src.dir,
		To:          spec.VMName,
		Network:     spec.Network,
		Backend:     spec.Backend,
		NoDirectIO:  spec.NoDirectIO,
		RestoreMode: restoreModeFor(p.RestoreMode, spec.OS),
		Pull:        src.snapshot != nil && src.snapshot.Image != "",
		CPUPolicy:   policy,
	}
	if shouldDropNICBeforeHibernate(spec) {
		opts.NICs = new(1)
	}
	v, err := p.Runtime.Clone(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("clone vm %s from %s: %w", spec.VMName, src.label(), err)
	}
	return v, nil
}

// dispatchHibernateRestore schedules post-restore: CH+Windows awaits the NIC's lease, others use runPostCloneSetup.
func (p *Provider) dispatchHibernateRestore(pod *corev1.Pod, spec meta.VMSpec, v *vm.VM, op string) {
	if shouldDropNICBeforeHibernate(spec) {
		p.goBackground(func() {
			p.markReadyAfterIP(p.lifecycleCtx, pod, v, true)
		})
		return
	}
	p.goBackground(func() {
		p.runPostCloneSetup(p.lifecycleCtx, pod, spec, v, "", op, true)
	})
}

// markLifecycleStateForWake gates on (tracked ∧ !hibernate ∧ VM==wakeVMID) and writes atomically.
func (p *Provider) markLifecycleStateForWake(ctx context.Context, pod *corev1.Pod, wakeVMID string, state meta.LifecycleState, message string) bool {
	status, applied := p.setLifecycleStateForWake(ctx, pod, wakeVMID, state, message)
	if applied {
		p.flushLifecycle(ctx, pod.Namespace, pod.Name, status)
	}
	return applied
}

func (p *Provider) setLifecycleStateForWake(ctx context.Context, pod *corev1.Pod, wakeVMID string, state meta.LifecycleState, message string) (meta.LifecycleStatus, bool) {
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	tracked, ok := p.pods[key]
	if !ok || bool(meta.ReadHibernateState(tracked)) {
		p.mu.Unlock()
		return meta.LifecycleStatus{}, false
	}
	if cur, ok := p.vmsByPod[key]; !ok || cur.ID != wakeVMID {
		p.mu.Unlock()
		return meta.LifecycleStatus{}, false
	}
	status, applied := p.applyLifecycleLocked(ctx, pod, state, message)
	p.mu.Unlock()
	return status, applied
}

func (p *Provider) markReadyPublishedForWake(ctx context.Context, pod *corev1.Pod, wakeVMID string) bool {
	status, applied := p.setLifecycleStateForWake(ctx, pod, wakeVMID, meta.LifecycleStateReady, "")
	if applied {
		p.publishReadyLifecycle(ctx, pod, status)
	}
	return applied
}

// waitForFreshIP polls for the clone's DHCP lease; UFFD contention under concurrent resumes can delay it many seconds.
func (p *Provider) waitForFreshIP(ctx context.Context, pod *corev1.Pod, vmID string) bool {
	budget := cmp.Or(p.wakeFreshIPBudget, defaultWakeFreshIPBudget)
	interval := cmp.Or(p.wakeFreshIPInterval, defaultWakeFreshIPInterval)
	deadline := time.Now().Add(budget)
	renewAt := time.Now().Add(cmp.Or(p.wakeRenewNudgeDelay, defaultWakeRenewNudgeDelay))
	nudged := meta.ParseVMSpec(pod).OS != string(cocoonv1.OSWindows)
	for {
		v := p.vmForPod(pod.Namespace, pod.Name)
		// A same-name recreate swaps the tracked VM; never touch the successor.
		if v == nil || v.ID != vmID {
			return false
		}
		if ip := p.resolveVMIP(pod.Namespace, pod.Name, v); ip != "" {
			return true
		}
		// One-shot renew: win11 can wedge on APIPA after a NAK and never re-DISCOVER.
		if !nudged && !time.Now().Before(renewAt) {
			nudged = true
			// Capped by the wake deadline so a hung agent cannot push the verdict past it.
			nudgeCtx, cancel := context.WithDeadline(ctx, deadline)
			err := p.execGuestIpconfig(nudgeCtx, v.ID, "renew")
			cancel()
			if err != nil {
				metrics.WakeRenewNudgeTotal.WithLabelValues("failed").Inc()
				log.WithFunc("Provider.waitForFreshIP").Warnf(ctx, "renew nudge %s/%s: %v", pod.Namespace, pod.Name, err)
			} else {
				metrics.WakeRenewNudgeTotal.WithLabelValues("ok").Inc()
			}
			// The lease may have landed during the exec; re-check before the deadline verdict.
			continue
		}
		if !time.Now().Before(deadline) {
			return false
		}
		if !commonk8s.SleepCtx(ctx, interval) {
			return false
		}
	}
}

// execGuestIpconfig runs `ipconfig /<verb>` over vsock, independent of the IP it is repairing.
func (p *Provider) execGuestIpconfig(ctx context.Context, vmID, verb string) error {
	ctx, cancel := context.WithTimeout(ctx, guestIpconfigTimeout)
	defer cancel()
	var out bytes.Buffer
	if err := p.Runtime.Exec(ctx, vmID, []string{"cmd", "/c", "ipconfig /" + verb}, nil, nil, &out, &out); err != nil {
		// Surface the guest-side reason ("The RPC server is unavailable", ...).
		if line := lastNonEmptyLine(out.String()); line != "" {
			return fmt.Errorf("%w: %s", err, line)
		}
		return err
	}
	return nil
}

// peerBaseURL resolves peerNode's InternalIP; every vk serves peers on the same port.
func (p *Provider) peerBaseURL(ctx context.Context, peerNode string) (string, error) {
	if p.PeerPort == "" {
		return "", errors.New("peer port not configured")
	}
	node, err := p.Clientset.CoreV1().Nodes().Get(ctx, peerNode, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return "http://" + net.JoinHostPort(addr.Address, p.PeerPort), nil
		}
	}
	return "", fmt.Errorf("node %s has no InternalIP address", peerNode)
}

// verifyLocalSnapshot compares the local and registry hibernate-tag SnapshotIDs; a mismatch means stale.
func (p *Provider) verifyLocalSnapshot(ctx context.Context, vmName string, local *vm.Snapshot) (*manifest.OCIManifest, *manifest.SnapshotConfig, error) {
	if p.Registry == nil {
		// Registry-less deployments never push: a local snapshot is current by construction.
		return nil, nil, nil
	}
	m, ok, err := p.fetchHibernateManifest(ctx, vmName)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, fmt.Errorf("%w: no hibernate tag in registry", errStaleLocalSnapshot)
	}
	cfg, err := commonsnapshot.FetchSnapshotConfig(ctx, p.Registry, vmName, m.Config)
	if err != nil {
		return m, nil, fmt.Errorf("fetch snapshot config: %w", err)
	}
	if cfg.SnapshotID != local.ID {
		return m, cfg, fmt.Errorf("%w: registry has %s, local is %s", errStaleLocalSnapshot, cfg.SnapshotID, local.ID)
	}
	return m, cfg, nil
}

// fetchHibernateManifest returns vmName's parsed hibernate-tag manifest; ok=false means no such tag (typed 404).
func (p *Provider) fetchHibernateManifest(ctx context.Context, vmName string) (*manifest.OCIManifest, bool, error) {
	raw, _, err := p.Registry.GetManifest(ctx, vmName, meta.HibernateSnapshotTag)
	switch {
	case errors.Is(err, commonsnapshot.ErrManifestNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("get hibernate manifest: %w", err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil, false, fmt.Errorf("parse hibernate manifest: %w", err)
	}
	return m, true, nil
}

func (p *Provider) forgetVMOnly(namespace, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropVMLocked(meta.PodKey(namespace, name))
}

// wakeSource is a resolved wake clone source: a named snapshot, or a peer-staged dir for `vm clone --from-dir`.
type wakeSource struct {
	localName string
	dir       string
	origin    string // display label when localName is empty (peer source)
	snapshot  *vm.Snapshot
	release   func()
}

func (s wakeSource) label() string { return cmp.Or(s.localName, s.origin) }

// resolveWakeSource picks the source in order: verified local snapshot, peer raw-file transfer, then registry pull.
func (p *Provider) resolveWakeSource(ctx context.Context, vmName string) (wakeSource, error) {
	var (
		m         *manifest.OCIManifest
		cfg       *manifest.SnapshotConfig
		tagAbsent bool
	)
	snapshot, err := p.Runtime.Snapshot(ctx, vmName)
	if err == nil {
		var verifyErr error
		switch m, cfg, verifyErr = p.verifyLocalSnapshot(ctx, vmName, snapshot); {
		case verifyErr == nil:
			metrics.SnapshotVerifyTotal.WithLabelValues("ok").Inc()
			return wakeSource{localName: vmName, snapshot: snapshot, release: func() {}}, nil
		case errors.Is(verifyErr, errStaleLocalSnapshot):
			metrics.SnapshotVerifyTotal.WithLabelValues("stale").Inc()
			log.WithFunc("Provider.resolveWakeSource").Warnf(ctx,
				"local snapshot %s is stale (%s), discarding and pulling", vmName, verifyErr)
			p.removeLocalSnapshots(ctx, vmName)
			tagAbsent = m == nil
		default:
			// Registry unreachable: never trust an unverified local copy.
			metrics.SnapshotVerifyTotal.WithLabelValues("error").Inc()
			return wakeSource{}, fmt.Errorf("verify local snapshot %s: %w", vmName, verifyErr)
		}
	} else if !errors.Is(err, vm.ErrSnapshotNotFound) {
		return wakeSource{}, fmt.Errorf("inspect local snapshot %s: %w", vmName, err)
	}
	if !tagAbsent {
		if src, ok := p.tryPeerRestore(ctx, vmName, m, cfg); ok {
			return src, nil
		}
	}
	if p.Puller == nil {
		return wakeSource{}, fmt.Errorf("wake %s: no local snapshot and no puller configured", vmName)
	}
	importName := vmName + hibernateImportSuffix
	pullStart := time.Now()
	if pullErr := p.Puller.PullSnapshot(ctx, vmName, meta.HibernateSnapshotTag, importName); pullErr != nil {
		metrics.SnapshotPullTotal.WithLabelValues("failed").Inc()
		return wakeSource{}, fmt.Errorf("pull hibernation snapshot %s: %w", vmName, pullErr)
	}
	metrics.SnapshotPullDuration.Observe(time.Since(pullStart).Seconds())
	metrics.SnapshotPullTotal.WithLabelValues("ok").Inc()
	snapshot, err = p.Runtime.Snapshot(ctx, importName)
	if err != nil {
		return wakeSource{}, fmt.Errorf("inspect imported snapshot %s: %w", importName, err)
	}
	return wakeSource{
		localName: importName,
		snapshot:  snapshot,
		// The import is a one-shot clone source; same-node wakes never get here.
		release: func() {
			p.goBackground(func() {
				p.removeSnapshotDetached(ctx, "Provider.wakeImportCleanup", importName)
			})
		},
	}, nil
}

// tryPeerRestore stages raw files from the manifest's peer node; best-effort, falls back to registry pull.
func (p *Provider) tryPeerRestore(ctx context.Context, vmName string, m *manifest.OCIManifest, cfg *manifest.SnapshotConfig) (wakeSource, bool) {
	if p.PeerRestorer == nil {
		return wakeSource{}, false
	}
	logger := log.WithFunc("Provider.tryPeerRestore")
	if m == nil {
		var ok bool
		var err error
		if m, ok, err = p.fetchHibernateManifest(ctx, vmName); err != nil {
			logger.Warnf(ctx, "peer restore %s: fetch hibernate manifest: %v (falling back)", vmName, err)
			return wakeSource{}, false
		} else if !ok {
			return wakeSource{}, false
		}
	}
	peerNode := m.Annotations[snapshots.AnnotationFromNode]
	if peerNode == "" || peerNode == p.NodeName {
		return wakeSource{}, false
	}
	if cfg == nil {
		var err error
		if cfg, err = commonsnapshot.FetchSnapshotConfig(ctx, p.Registry, vmName, m.Config); err != nil {
			logger.Warnf(ctx, "peer restore %s: fetch registry config: %v (falling back)", vmName, err)
			return wakeSource{}, false
		}
	}
	baseURL, err := p.peerBaseURL(ctx, peerNode)
	if err != nil {
		logger.Warnf(ctx, "peer restore %s: resolve %s: %v (falling back)", vmName, peerNode, err)
		return wakeSource{}, false
	}
	start := time.Now()
	restored, cleanup, err := p.PeerRestorer.Restore(ctx, baseURL, vmName, cfg)
	if err != nil {
		metrics.PeerRestoreTotal.WithLabelValues("failed").Inc()
		logger.Warnf(ctx, "peer restore %s from %s: %v (falling back to registry)", vmName, peerNode, err)
		return wakeSource{}, false
	}
	metrics.PeerRestoreDuration.Observe(time.Since(start).Seconds())
	metrics.PeerRestoreTotal.WithLabelValues("ok").Inc()
	logger.Infof(ctx, "peer restore %s from %s in %.1fs", vmName, peerNode, time.Since(start).Seconds())
	return wakeSource{dir: restored.Dir, origin: "peer " + peerNode, snapshot: restored.Snapshot, release: cleanup}, true
}

// lastNonEmptyLine returns the last non-blank line of s, trimmed; ipconfig prints its actual error last.
func lastNonEmptyLine(s string) string {
	last := ""
	for line := range strings.SplitSeq(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			last = trimmed
		}
	}
	return last
}

// shouldDropNICBeforeHibernate: Windows PnP rejects MAC swap on the same slot; only CH implements `vm net --nics`.
func shouldDropNICBeforeHibernate(spec meta.VMSpec) bool {
	return spec.Backend == string(cocoonv1.BackendCloudHypervisor) &&
		spec.OS == string(cocoonv1.OSWindows)
}
