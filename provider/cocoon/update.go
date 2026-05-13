package cocoon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// hibernateImportSuffix avoids name collision with the live VM the Clone produces.
const hibernateImportSuffix = "-hibernate-import"

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
			metrics.PodLifecycleTotal.WithLabelValues("update", "hibernate_failed").Inc()
			logger.Errorf(ctx, err, "hibernate %s/%s", pod.Namespace, pod.Name)
			return err
		}
		metrics.PodLifecycleTotal.WithLabelValues("update", "hibernated").Inc()
	case !wantHibernate && v == nil:
		if err := p.wake(ctx, pod); err != nil {
			metrics.PodLifecycleTotal.WithLabelValues("update", "wake_failed").Inc()
			logger.Errorf(ctx, err, "wake %s/%s", pod.Namespace, pod.Name)
			return err
		}
		metrics.PodLifecycleTotal.WithLabelValues("update", "woken").Inc()
	default:
		// Skip refresh+notify so we don't echo the incoming pod back.
		p.republishLifecycleOnGenerationBump(ctx, pod)
		metrics.PodLifecycleTotal.WithLabelValues("update", "noop").Inc()
		return nil
	}
	p.refreshStatus(ctx, pod)
	p.notify(pod)
	return nil
}

// hibernate runs Save -> Push -> Remove. CH+Windows drops the NIC first
// so wake can hot-add a fresh device and bypass Windows PnP MAC-swap.
// VMID clears pre-Save so operator never sees manifest+VMID together.
func (p *Provider) hibernate(ctx context.Context, pod *corev1.Pod, v *vm.VM) error {
	logger := log.WithFunc("Provider.hibernate")
	spec := meta.ParseVMSpec(pod)
	p.markLifecycleState(ctx, pod, meta.LifecycleStateHibernating, "")
	if err := p.clearRuntimeAnnotations(ctx, pod); err != nil {
		logger.Warnf(ctx, "clear pre-hibernate annotations %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	if shouldDropNICBeforeHibernate(spec) {
		if err := p.Runtime.NetResize(ctx, v.ID, 0); err != nil {
			err = fmt.Errorf("drop NIC pre-hibernate %s: %w", v.Name, err)
			p.markLifecycleState(ctx, pod, meta.LifecycleStateFailed, err.Error())
			return err
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
			if delErr := p.Registry.DeleteManifest(ctx, v.Name, meta.HibernateSnapshotTag); delErr != nil {
				logger.Errorf(ctx, delErr, "rollback hibernate push after remove failed for %s", v.Name)
			}
		}
		err = fmt.Errorf("remove vm %s: %w", v.ID, err)
		p.markLifecycleState(ctx, pod, meta.LifecycleStateFailed, err.Error())
		return err
	}
	// Safety-net for the rare pre-Save patch failure.
	if err := p.clearRuntimeAnnotations(ctx, pod); err != nil {
		logger.Errorf(ctx, err, "clear hibernate annotations %s/%s (VM already removed)", pod.Namespace, pod.Name)
	}
	p.forgetVMOnly(pod.Namespace, pod.Name)
	p.markLifecycleState(ctx, pod, meta.LifecycleStateHibernated, "")
	return nil
}

// wake restores the VM from the hibernation snapshot. CH+Windows clones
// with --nics 1 and skips post-clone setup (NIC-less snapshot invariant).
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
		OnDemand:   useOnDemandClone(spec.OS),
	}
	if dropNIC {
		opts.NICs = ptr.To(1)
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
	p.cleanupWakeImport(spec.VMName, sourceName)
	if dropNIC {
		p.markLifecycleState(ctx, pod, meta.LifecycleStateReady, "")
	} else {
		p.goBackground(func() {
			p.runPostCloneSetup(p.lifecycleCtx, pod, spec, v, "")
		})
	}
	p.trackPod(pod, v)
	p.startProbeIfEnabled(pod)
	return nil
}

// resolveWakeSource returns the local snapshot when present, else pulls.
func (p *Provider) resolveWakeSource(ctx context.Context, vmName string) (string, error) {
	_, err := p.Runtime.Snapshot(ctx, vmName)
	if err == nil {
		return vmName, nil
	}
	if !errors.Is(err, vm.ErrSnapshotNotFound) {
		return "", fmt.Errorf("inspect local snapshot %s: %w", vmName, err)
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

// shouldDropNICBeforeHibernate: Windows PnP rejects MAC swap on the same
// PCI slot, and only CH implements `vm net --nics`.
func shouldDropNICBeforeHibernate(spec meta.VMSpec) bool {
	return spec.Backend == string(cocoonv1.BackendCloudHypervisor) &&
		spec.OS == string(cocoonv1.OSWindows)
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
