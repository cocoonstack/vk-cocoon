package main

import (
	"context"
	"fmt"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// hibernateImportSuffix is appended to the VM name when wake-time
// PullSnapshot imports the hibernation tar back from epoch. The
// import target needs a different name from the live VM that the
// subsequent Clone produces, otherwise cocoon snapshot import and
// cocoon vm clone collide on the same name.
const hibernateImportSuffix = "-hibernate-import"

// UpdatePod is the virtual-kubelet entry point for in-place pod
// updates. The only update vk-cocoon honors is a HibernateState
// transition: when the operator (via CocoonHibernation) flips the
// hibernate annotation we either snapshot + tear down (true) or
// restore (false). Other spec changes are ignored — the operator is
// expected to delete and recreate the pod for anything else.
func (p *CocoonProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("CocoonProvider.UpdatePod")
	logger.Infof(ctx, "update pod %s/%s", pod.Namespace, pod.Name)

	// Refresh the in-memory copy so subsequent GetPod returns the
	// latest spec / annotations.
	v := p.vmForPod(pod.Namespace, pod.Name)
	p.trackPod(pod, v)

	desire := bool(meta.ReadHibernateState(pod))

	switch {
	case desire && v != nil:
		if err := p.hibernate(ctx, pod, v); err != nil {
			metrics.PodLifecycleTotal.WithLabelValues("update", "hibernate_failed").Inc()
			return err
		}
		metrics.PodLifecycleTotal.WithLabelValues("update", "hibernated").Inc()
	case !desire && v == nil:
		// Wake path: hibernate annotation cleared but the VM is gone.
		// Recreate it from the snapshot tag epoch knows about.
		if err := p.wake(ctx, pod); err != nil {
			metrics.PodLifecycleTotal.WithLabelValues("update", "wake_failed").Inc()
			return err
		}
		metrics.PodLifecycleTotal.WithLabelValues("update", "woken").Inc()
	default:
		metrics.PodLifecycleTotal.WithLabelValues("update", "noop").Inc()
	}
	p.refreshStatus(ctx, pod)
	p.notify(pod)
	return nil
}

// hibernate snapshots the VM into epoch under the well-known
// hibernate tag, then tears it down. The pod stays alive (vk-cocoon
// keeps the container in PodRunning) so K8s controllers do not
// recreate it; the operator detects the snapshot via
// epoch.GetManifest and marks the CocoonHibernation as Hibernated.
//
// Ordering is Save → Push → Remove with a compensating rollback on
// Remove failure. The naive order (Save → Push → Remove without
// rollback) is wrong: the operator's HasManifest probe fires the
// moment Push succeeds, so a failed Remove would let the operator
// observe Hibernated while the local VM is still running. If Remove
// fails we best-effort DeleteManifest the tag we just published so
// the next operator probe sees it absent and the
// CocoonHibernation stays at Hibernating until a retry completes.
// Push and Save are both idempotent — a compensated retry will
// re-publish the tag and re-attempt Remove cleanly.
func (p *CocoonProvider) hibernate(ctx context.Context, pod *corev1.Pod, v *vm.VM) error {
	logger := log.WithFunc("CocoonProvider.hibernate")
	if err := p.Runtime.SnapshotSave(ctx, v.Name, v.ID); err != nil {
		return fmt.Errorf("snapshot save %s: %w", v.Name, err)
	}
	if p.Pusher != nil {
		if _, err := p.Pusher.PushSnapshot(ctx, v.Name, v.Name, meta.HibernateSnapshotTag, ""); err != nil {
			return fmt.Errorf("push hibernation snapshot %s: %w", v.Name, err)
		}
	}
	if err := p.Runtime.Remove(ctx, v.ID); err != nil {
		if p.Registry != nil {
			// Compensating transaction: drop the hibernate tag so
			// the operator does not advance to Hibernated with a
			// live VM still on the host. A rollback error here
			// leaves drift, so log at Error so oncall sees it.
			if delErr := p.Registry.DeleteManifest(ctx, v.Name, meta.HibernateSnapshotTag); delErr != nil {
				logger.Errorf(ctx, delErr, "rollback hibernate push after remove failed for %s", v.Name)
			}
		}
		return fmt.Errorf("remove vm %s: %w", v.ID, err)
	}
	// Clear the runtime annotations: the VM no longer exists, but
	// the pod and the VMSpec stay so the wake path knows what to
	// restore. Use delete so absence-as-default reads cleanly,
	// rather than writing empty strings that ParseVMRuntime would
	// dutifully decode as a present-but-empty record.
	if pod.Annotations != nil {
		delete(pod.Annotations, meta.AnnotationVMID)
		delete(pod.Annotations, meta.AnnotationIP)
	}
	p.forgetVMOnly(pod.Namespace, pod.Name)
	return nil
}

// wake restores the VM from the hibernation snapshot tag and
// re-tracks it in the in-memory tables.
func (p *CocoonProvider) wake(ctx context.Context, pod *corev1.Pod) error {
	spec := meta.ParseVMSpec(pod)
	if spec.VMName == "" {
		return nil
	}
	if p.Puller == nil {
		// Without a puller we cannot import the hibernation tar
		// epoch holds, and the subsequent Clone would fail with a
		// confusing "snapshot not found". Surface the wiring gap
		// up-front instead.
		return fmt.Errorf("wake %s: no snapshot puller configured", spec.VMName)
	}
	cpu, memory := vmResourceOverrides(pod)
	importName := spec.VMName + hibernateImportSuffix
	if err := p.Puller.PullSnapshot(ctx, spec.VMName, meta.HibernateSnapshotTag, importName); err != nil {
		return fmt.Errorf("pull hibernation snapshot %s: %w", spec.VMName, err)
	}
	v, err := p.Runtime.Clone(ctx, vm.CloneOptions{
		From:    importName,
		To:      spec.VMName,
		CPU:     cpu,
		Memory:  memory,
		Network: spec.Network,
		Storage: spec.Storage,
	})
	if err != nil {
		return fmt.Errorf("clone vm %s from %s: %w", spec.VMName, importName, err)
	}
	p.applyRuntime(pod, v)
	p.trackPod(pod, v)
	if p.Registry != nil {
		// Best-effort: drop the hibernation tag now that we have
		// successfully restored. The operator's wake reconcile also
		// tries this so a stale tag is not a hard failure.
		_ = p.Registry.DeleteManifest(ctx, spec.VMName, meta.HibernateSnapshotTag)
	}
	return nil
}

// forgetVMOnly clears the VM record but leaves the pod in the
// in-memory table; used by hibernate to keep the pod alive while
// the underlying VM is destroyed.
func (p *CocoonProvider) forgetVMOnly(namespace, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropVMLocked(meta.PodKey(namespace, name))
}
