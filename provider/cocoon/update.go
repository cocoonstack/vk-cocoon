package cocoon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// hibernateImportSuffix avoids name collision between the import target
// and the live VM that the subsequent Clone produces.
const hibernateImportSuffix = "-hibernate-import"

// UpdatePod handles hibernate/wake transitions. Other spec changes are ignored.
// A K8s-side pod update that does not represent a hibernate/wake toggle is a
// no-op: refreshing status and re-notifying on every tick just feeds a loop
// where the patched pod comes back as another UpdatePod, pinning CPU and
// tripling the reconcile work the operator has to do.
func (p *Provider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("Provider.UpdatePod")
	logger.Infof(ctx, "update pod %s/%s", pod.Namespace, pod.Name)

	v := p.vmForPod(pod.Namespace, pod.Name)
	p.trackPod(pod, v)

	wantHibernate := bool(meta.ReadHibernateState(pod))

	switch {
	case wantHibernate && v != nil:
		if err := p.hibernate(ctx, pod, v); err != nil {
			// Surface the wrapped error so operators can see why the workqueue keeps retrying.
			metrics.PodLifecycleTotal.WithLabelValues("update", "hibernate_failed").Inc()
			logger.Errorf(ctx, err, "hibernate %s/%s", pod.Namespace, pod.Name)
			return err
		}
		metrics.PodLifecycleTotal.WithLabelValues("update", "hibernated").Inc()
	case !wantHibernate && v == nil:
		// Wake: recreate from the hibernation snapshot.
		if err := p.wake(ctx, pod); err != nil {
			metrics.PodLifecycleTotal.WithLabelValues("update", "wake_failed").Inc()
			logger.Errorf(ctx, err, "wake %s/%s", pod.Namespace, pod.Name)
			return err
		}
		metrics.PodLifecycleTotal.WithLabelValues("update", "woken").Inc()
	default:
		// No lifecycle transition; skip the refresh+notify round trip so we
		// don't echo the incoming pod back to the apiserver.
		p.republishLifecycleOnGenerationBump(ctx, pod)
		metrics.PodLifecycleTotal.WithLabelValues("update", "noop").Inc()
		return nil
	}
	p.refreshStatus(ctx, pod)
	p.notify(pod)
	return nil
}

// hibernate snapshots the VM to epoch then tears it down. Order is
// Save -> Push -> Remove. If Remove fails, the pushed tag is rolled back
// so the operator does not observe Hibernated while the VM is still running.
//
// CH+Windows runs `vm net --nics 0` first so the snapshot captures a NIC-less
// guest. The matched wake clone uses --nics 1 to hot-add a fresh NIC, which
// Windows enumerates as a brand-new device — bypassing the MAC-swap PnP path
// that previously forced an in-guest powershell rebind.
func (p *Provider) hibernate(ctx context.Context, pod *corev1.Pod, v *vm.VM) error {
	logger := log.WithFunc("Provider.hibernate")
	spec := meta.ParseVMSpec(pod)
	p.markLifecycleState(ctx, pod, meta.LifecycleStateHibernating, "")
	if shouldDropNICBeforeHibernate(spec) {
		if err := p.Runtime.NetResize(ctx, v.ID, 0); err != nil {
			if errors.Is(err, vm.ErrNetResizeUnsupported) {
				logger.Warnf(ctx, "drop NIC pre-hibernate of %s: backend lacks net resize; snapshot keeps existing NICs", v.Name)
			} else {
				logger.Warnf(ctx, "drop NIC pre-hibernate of %s failed: %v — continuing with original NIC count", v.Name, err)
			}
		}
	}
	saveStart := time.Now()
	if err := p.Runtime.SnapshotSave(ctx, v.Name, v.ID); err != nil {
		metrics.SnapshotSaveTotal.WithLabelValues("failed").Inc()
		err = fmt.Errorf("snapshot save %s: %w", v.Name, err)
		p.markLifecycleState(ctx, pod, meta.LifecycleStateFailed, err.Error())
		return err
	}
	metrics.SnapshotSaveDuration.Observe(time.Since(saveStart).Seconds())
	metrics.SnapshotSaveTotal.WithLabelValues("ok").Inc()
	if p.Pusher != nil {
		pushStart := time.Now()
		if _, err := p.Pusher.PushSnapshot(ctx, v.Name, v.Name, meta.HibernateSnapshotTag, ""); err != nil {
			metrics.SnapshotPushTotal.WithLabelValues("failed").Inc()
			err = fmt.Errorf("push hibernation snapshot %s: %w", v.Name, err)
			p.markLifecycleState(ctx, pod, meta.LifecycleStateFailed, err.Error())
			return err
		}
		metrics.SnapshotPushDuration.Observe(time.Since(pushStart).Seconds())
		metrics.SnapshotPushTotal.WithLabelValues("ok").Inc()
	}
	if err := p.Runtime.Remove(ctx, v.ID); err != nil {
		if p.Registry != nil {
			// Roll back the hibernate tag.
			if delErr := p.Registry.DeleteManifest(ctx, v.Name, meta.HibernateSnapshotTag); delErr != nil {
				logger.Errorf(ctx, delErr, "rollback hibernate push after remove failed for %s", v.Name)
			}
		}
		err = fmt.Errorf("remove vm %s: %w", v.ID, err)
		p.markLifecycleState(ctx, pod, meta.LifecycleStateFailed, err.Error())
		return err
	}
	// Best-effort: VM is already removed, so retrying the whole hibernate
	// on patch failure would hit "VM not found". Log and continue.
	// Startup reconcile detects the stale state via VMID + hibernate annotation.
	if err := p.clearRuntimeAnnotations(ctx, pod); err != nil {
		logger.Errorf(ctx, err, "clear hibernate annotations %s/%s (VM already removed)", pod.Namespace, pod.Name)
	}
	p.forgetVMOnly(pod.Namespace, pod.Name)
	p.markLifecycleState(ctx, pod, meta.LifecycleStateHibernated, "")
	return nil
}

// wake restores the VM from the hibernation snapshot. The CH+Windows path
// is the inverse of the drop-NIC hibernate: the snapshot has zero NICs, so
// clone overrides with --nics 1 to hot-add a fresh device. The same path
// skips runPostCloneSetup because a fresh NIC needs no PnP rebind — there
// is no prior MAC for Windows to be confused about.
func (p *Provider) wake(ctx context.Context, pod *corev1.Pod) error {
	spec := meta.ParseVMSpec(pod)
	if spec.VMName == "" {
		return nil
	}
	p.markLifecycleState(ctx, pod, meta.LifecycleStateCreating, "")
	sourceName, err := p.resolveWakeSource(ctx, spec.VMName)
	if err != nil {
		p.markLifecycleState(ctx, pod, meta.LifecycleStateFailed, err.Error())
		return err
	}
	dropNIC := shouldDropNICBeforeHibernate(spec)
	opts := vm.CloneOptions{
		From:       sourceName,
		To:         spec.VMName,
		Network:    spec.Network,
		Backend:    spec.Backend,
		NoDirectIO: spec.NoDirectIO,
		OnDemand:   true,
	}
	if dropNIC {
		opts.NICs = ptr(1)
	}
	cloneStart := time.Now()
	v, err := p.Runtime.Clone(ctx, opts)
	if err != nil {
		err = fmt.Errorf("clone vm %s from %s: %w", spec.VMName, sourceName, err)
		p.markLifecycleState(ctx, pod, meta.LifecycleStateFailed, err.Error())
		return err
	}
	metrics.VMBootDuration.WithLabelValues("clone", spec.Backend).Observe(time.Since(cloneStart).Seconds())
	p.applyRuntime(ctx, pod, v)
	if dropNIC {
		p.markLifecycleState(ctx, pod, meta.LifecycleStateReady, "")
	} else {
		p.goBackground(func() {
			p.runPostCloneSetup(p.lifecycleCtx, pod, spec, v, "")
		})
	}
	p.trackPod(pod, v)
	p.startProbeIfEnabled(pod)
	// Hibernate tag cleanup is the operator's responsibility (reconcileWake).
	return nil
}

// resolveWakeSource returns the local snapshot name to clone from on wake.
// Same-node hibernate→wake hits the local snapshot left behind by hibernate's
// SnapshotSave and skips the epoch round-trip; cross-node falls back to pull.
func (p *Provider) resolveWakeSource(ctx context.Context, vmName string) (string, error) {
	if _, err := p.Runtime.Snapshot(ctx, vmName); err == nil {
		return vmName, nil
	}
	if p.Puller == nil {
		return "", fmt.Errorf("wake %s: no local snapshot and no puller configured", vmName)
	}
	importName := vmName + hibernateImportSuffix
	pullStart := time.Now()
	if err := p.Puller.PullSnapshot(ctx, vmName, meta.HibernateSnapshotTag, importName); err != nil {
		metrics.SnapshotPullTotal.WithLabelValues("failed").Inc()
		return "", fmt.Errorf("pull hibernation snapshot %s: %w", vmName, err)
	}
	metrics.SnapshotPullDuration.Observe(time.Since(pullStart).Seconds())
	metrics.SnapshotPullTotal.WithLabelValues("ok").Inc()
	return importName, nil
}

// ptr returns a pointer to v. Used to populate optional *T fields inline
// without spilling a one-shot variable into the surrounding scope.
func ptr[T any](v T) *T { return &v }

// shouldDropNICBeforeHibernate reports whether the hibernate path should
// run `vm net --nics 0` before snapshot save (and the matching wake path
// should re-add the NIC via `vm clone --nics 1`). Confined to CH+Windows:
// Windows PnP cannot tolerate MAC swap on the same PCI slot, and CH is
// the only backend whose `vm net --nics` extension is implemented.
func shouldDropNICBeforeHibernate(spec meta.VMSpec) bool {
	return spec.Backend == string(cocoonv1.BackendCloudHypervisor) &&
		spec.OS == string(cocoonv1.OSWindows)
}

// forgetVMOnly clears the VM record but keeps the pod (used by hibernate).
func (p *Provider) forgetVMOnly(namespace, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropVMLocked(meta.PodKey(namespace, name))
}
