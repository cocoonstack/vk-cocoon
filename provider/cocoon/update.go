package cocoon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	// hibernateImportSuffix avoids name collision with the live VM the Clone produces.
	hibernateImportSuffix = "-hibernate-import"

	// defaultWakeFreshIPBudget caps the wait for a fresh DHCP lease after a clone
	// re-DHCPs on a new NIC/MAC: dropNIC wake, and every fork/restore clone routed
	// through markReadyAfterIP. On-demand clones fault RAM in lazily over UFFD and
	// concurrent resumes (replicas>1) contend, so the first lease can land many
	// seconds after resume — long enough to surface as a spurious lifecycle=failed
	// and an operator sub-agent rebuild. This budget absorbs that; a genuinely dead
	// VM still fails, just later.
	defaultWakeFreshIPBudget   = 45 * time.Second
	defaultWakeFreshIPInterval = 200 * time.Millisecond
)

// UpdatePod handles hibernate/wake transitions; other spec changes are no-ops
// to avoid echoing the patched pod back as another UpdatePod.
func (p *Provider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("Provider.UpdatePod")
	logger.Infof(ctx, "update pod %s/%s", pod.Namespace, pod.Name)

	v := p.vmForPod(pod.Namespace, pod.Name)
	p.trackPod(pod, v)

	wantHibernate := bool(meta.ReadHibernateState(pod))

	switch {
	case wantHibernate && v != nil:
		if err := p.hibernate(ctx, pod, v); err != nil {
			return err
		}
		metrics.PodLifecycleTotal.WithLabelValues("update", "ok", "").Inc()
	case !wantHibernate && v == nil:
		if err := p.wake(ctx, pod); err != nil {
			return err
		}
		metrics.PodLifecycleTotal.WithLabelValues("update", "ok", "").Inc()
	default:
		// Skip refresh+notify so we don't echo the incoming pod back.
		p.republishLifecycleOnGenerationBump(ctx, pod)
		metrics.PodLifecycleTotal.WithLabelValues("update", "skipped", "noop").Inc()
		return nil
	}
	p.refreshStatus(ctx, pod)
	p.notify(pod)
	return nil
}

// hibernate runs Save -> Push -> Remove. CH+Windows drops the NIC first
// so wake can hot-add a fresh device and bypass Windows PnP MAC-swap.
// VMID clears between Push and Remove so the operator's manifest+VMID
// race window collapses to one patch RTT; failures before that point
// keep VMID intact so the pod stays recoverable.
func (p *Provider) hibernate(ctx context.Context, pod *corev1.Pod, v *vm.VM) error {
	logger := log.WithFunc("Provider.hibernate")
	spec := meta.ParseVMSpec(pod)
	p.markLifecycleState(ctx, pod, meta.LifecycleStateHibernating, "")
	dropNIC := shouldDropNICBeforeHibernate(spec)
	if dropNIC {
		if err := p.Runtime.NetResize(ctx, v.ID, 0); err != nil {
			metrics.HibernateTotal.WithLabelValues("netresize", "failed").Inc()
			err = fmt.Errorf("drop NIC pre-hibernate %s: %w", v.Name, err)
			p.failOp(ctx, pod, "HibernateNetResizeFailed", "update", err)
			return err
		}
		metrics.HibernateTotal.WithLabelValues("netresize", "ok").Inc()
	}
	saveStart := time.Now()
	if err := p.Runtime.SnapshotSave(ctx, v.Name, v.ID); err != nil {
		metrics.SnapshotSaveTotal.WithLabelValues("failed").Inc()
		metrics.HibernateTotal.WithLabelValues("snapshot", "failed").Inc()
		p.rollbackHibernateNIC(ctx, v, dropNIC)
		err = fmt.Errorf("snapshot save %s: %w", v.Name, err)
		p.failOp(ctx, pod, "HibernateSnapshotFailed", "update", err)
		return err
	}
	metrics.SnapshotSaveDuration.Observe(time.Since(saveStart).Seconds())
	metrics.SnapshotSaveTotal.WithLabelValues("ok").Inc()
	metrics.HibernateTotal.WithLabelValues("snapshot", "ok").Inc()
	if p.Pusher != nil {
		pushStart := time.Now()
		if _, err := p.Pusher.PushSnapshot(ctx, v.Name, v.Name, meta.HibernateSnapshotTag, ""); err != nil {
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
	if err := p.Runtime.Remove(ctx, v.ID); err != nil {
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

// rollbackHibernateNIC re-adds the NIC dropped pre-snapshot.
func (p *Provider) rollbackHibernateNIC(ctx context.Context, v *vm.VM, dropped bool) {
	if !dropped {
		return
	}
	if err := p.Runtime.NetResize(ctx, v.ID, 1); err != nil {
		log.WithFunc("Provider.rollbackHibernateNIC").Errorf(ctx, err, "re-add NIC after hibernate failure %s", v.Name)
	}
}

// wake restores the VM from the hibernation snapshot. CH+Windows defers
// Ready to finalizeDropNICWake; other backends fall through to runPostCloneSetup.
func (p *Provider) wake(ctx context.Context, pod *corev1.Pod) error {
	spec := meta.ParseVMSpec(pod)
	if spec.VMName == "" {
		return nil
	}
	p.markLifecycleState(ctx, pod, meta.LifecycleStateCreating, "")
	sourceName, snapshot, err := p.resolveWakeSource(ctx, spec.VMName)
	if err != nil {
		metrics.WakeTotal.WithLabelValues("failed").Inc()
		p.failOp(ctx, pod, "WakePullFailed", "update", err)
		return err
	}
	cloneStart := time.Now()
	v, err := p.cloneFromHibernate(ctx, spec, sourceName, snapshot)
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
	p.emitNormalf(pod, "Woken", "cloned from %s", sourceName)
	return nil
}

// cloneFromHibernate clones the VM from an already-resolved hibernate snapshot
// source. CH+Windows hibernate snapshots are captured NIC-less, so the clone
// hot-adds a fresh NIC that Windows enumerates as new hardware. The local import
// copy (cross-node pull) is dropped whether the clone succeeds or fails.
func (p *Provider) cloneFromHibernate(ctx context.Context, spec meta.VMSpec, sourceName string, snapshot *vm.Snapshot) (*vm.VM, error) {
	defer p.cleanupWakeImport(spec.VMName, sourceName)
	// Same guard as the fresh-clone path: `vm clone --pull` fetches only
	// http(s) bases, so an OCI-ref base absent from the local store must be
	// materialized here or the restore fails resolving the backing file.
	if snapshot != nil && snapshot.Image != "" && !isHTTPURL(snapshot.Image) && !p.imagePresent(ctx, snapshot.ImageDigest) {
		if _, imgErr := p.ensureRunImage(ctx, snapshot.Image, false); imgErr != nil {
			return nil, fmt.Errorf("ensure restore base image %s: %w", snapshot.Image, imgErr)
		}
	}
	opts := vm.CloneOptions{
		From:        sourceName,
		To:          spec.VMName,
		Network:     spec.Network,
		Backend:     spec.Backend,
		NoDirectIO:  spec.NoDirectIO,
		RestoreMode: restoreModeFor(p.RestoreMode, spec.OS),
		Pull:        snapshot != nil && snapshot.Image != "",
	}
	if shouldDropNICBeforeHibernate(spec) {
		opts.NICs = ptr.To(1)
	}
	v, err := p.Runtime.Clone(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("clone vm %s from %s: %w", spec.VMName, sourceName, err)
	}
	return v, nil
}

// dispatchHibernateRestore schedules the post-restore step. A CH+Windows restore
// hot-added a fresh NIC, so Ready waits on that NIC's DHCP lease (no PnP rebind);
// other backends re-derive networking via runPostCloneSetup.
func (p *Provider) dispatchHibernateRestore(pod *corev1.Pod, spec meta.VMSpec, v *vm.VM, op string) {
	if shouldDropNICBeforeHibernate(spec) {
		p.goBackground(func() {
			p.finalizeDropNICWake(p.lifecycleCtx, pod, v)
		})
		return
	}
	// A restore counts as a wake regardless of trigger (cross-node create or update).
	metrics.WakeTotal.WithLabelValues("ok").Inc()
	p.goBackground(func() {
		p.runPostCloneSetup(p.lifecycleCtx, pod, spec, v, "", op)
	})
}

// finalizeDropNICWake holds Ready until the fresh NIC's lease lands.
func (p *Provider) finalizeDropNICWake(ctx context.Context, pod *corev1.Pod, v *vm.VM) {
	gotIP := p.waitForFreshIP(ctx, pod.Namespace, pod.Name)
	if ctx.Err() != nil {
		return
	}
	if gotIP {
		p.refreshStatus(ctx, pod)
		p.notify(pod)
		if p.markLifecycleStateForWake(ctx, pod, v.ID, meta.LifecycleStateReady, "") {
			metrics.WakeIPWaitTotal.WithLabelValues("ok").Inc()
			metrics.WakeTotal.WithLabelValues("ok").Inc()
		}
		return
	}
	budget := cmp.Or(p.wakeFreshIPBudget, defaultWakeFreshIPBudget)
	err := fmt.Errorf("wake %s: dhcp lease for fresh NIC not observed within %s", v.Name, budget)
	msg := err.Error()
	if p.markLifecycleStateForWake(ctx, pod, v.ID, meta.LifecycleStateFailed, truncate(msg, lifecycleMessageMaxBytes)) {
		metrics.WakeIPWaitTotal.WithLabelValues("timeout").Inc()
		metrics.WakeTotal.WithLabelValues("failed").Inc()
		p.emitWarningf(pod, "WakeIPWaitTimeout", "%s", truncate("update: "+msg, eventMessageMaxBytes))
		log.WithFunc("Provider.finalizeDropNICWake").Errorf(ctx, err, "%s/%s update", pod.Namespace, pod.Name)
	}
}

// markLifecycleStateForWake gates on (pod tracked) ∧ (hibernate not requested) ∧ (VM
// matches wakeVMID) and writes atomically; callers gate side effects on the return.
func (p *Provider) markLifecycleStateForWake(ctx context.Context, pod *corev1.Pod, wakeVMID string, state meta.LifecycleState, message string) bool {
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	tracked, ok := p.pods[key]
	if !ok || bool(meta.ReadHibernateState(tracked)) {
		p.mu.Unlock()
		return false
	}
	if cur, ok := p.vmsByPod[key]; !ok || cur.ID != wakeVMID {
		p.mu.Unlock()
		return false
	}
	status, applied := p.applyLifecycleLocked(ctx, pod, state, message)
	p.mu.Unlock()
	if !applied {
		return false
	}
	p.flushLifecycle(ctx, pod.Namespace, pod.Name, status)
	return true
}

func (p *Provider) waitForFreshIP(ctx context.Context, namespace, name string) bool {
	budget := cmp.Or(p.wakeFreshIPBudget, defaultWakeFreshIPBudget)
	interval := cmp.Or(p.wakeFreshIPInterval, defaultWakeFreshIPInterval)
	deadline := time.Now().Add(budget)
	for {
		v := p.vmForPod(namespace, name)
		if v == nil {
			return false
		}
		if ip := p.resolveVMIP(namespace, name, v); ip != "" {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		if !commonk8s.SleepCtx(ctx, interval) {
			return false
		}
	}
}

// resolveWakeSource returns the clone source name and its snapshot metadata —
// the local snapshot when present, else pulled from the registry.
func (p *Provider) resolveWakeSource(ctx context.Context, vmName string) (string, *vm.Snapshot, error) {
	snapshot, err := p.Runtime.Snapshot(ctx, vmName)
	if err == nil {
		return vmName, snapshot, nil
	}
	if !errors.Is(err, vm.ErrSnapshotNotFound) {
		return "", nil, fmt.Errorf("inspect local snapshot %s: %w", vmName, err)
	}
	if p.Puller == nil {
		return "", nil, fmt.Errorf("wake %s: no local snapshot and no puller configured", vmName)
	}
	importName := vmName + hibernateImportSuffix
	pullStart := time.Now()
	if pullErr := p.Puller.PullSnapshot(ctx, vmName, meta.HibernateSnapshotTag, importName); pullErr != nil {
		metrics.SnapshotPullTotal.WithLabelValues("failed").Inc()
		return "", nil, fmt.Errorf("pull hibernation snapshot %s: %w", vmName, pullErr)
	}
	metrics.SnapshotPullDuration.Observe(time.Since(pullStart).Seconds())
	metrics.SnapshotPullTotal.WithLabelValues("ok").Inc()
	snapshot, err = p.Runtime.Snapshot(ctx, importName)
	if err != nil {
		return "", nil, fmt.Errorf("inspect imported snapshot %s: %w", importName, err)
	}
	return importName, snapshot, nil
}

// cleanupWakeImport drops the cross-node import; same-node keeps the
// local snapshot live for the next wake.
func (p *Provider) cleanupWakeImport(vmName, sourceName string) {
	if sourceName == vmName {
		return
	}
	p.goBackground(func() {
		p.removeSnapshotDetached("Provider.cleanupWakeImport", sourceName)
	})
}

// forgetVMOnly clears the VM record but keeps the pod.
func (p *Provider) forgetVMOnly(namespace, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropVMLocked(meta.PodKey(namespace, name))
}

// shouldDropNICBeforeHibernate: Windows PnP rejects MAC swap on the same
// PCI slot, and only CH implements `vm net --nics`.
func shouldDropNICBeforeHibernate(spec meta.VMSpec) bool {
	return spec.Backend == string(cocoonv1.BackendCloudHypervisor) &&
		spec.OS == string(cocoonv1.OSWindows)
}
