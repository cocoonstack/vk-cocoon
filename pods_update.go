package main

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// UpdatePod is the virtual-kubelet entry point for in-place pod
// updates. The only update vk-cocoon honors is a HibernateState
// transition: when the operator (via CocoonHibernation) flips the
// hibernate annotation we either snapshot + tear down (true) or
// restore (false). Other spec changes are ignored — the operator is
// expected to delete and recreate the pod for anything else.
func (p *CocoonProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	logger := providerLogger("UpdatePod")
	logger.Infof(ctx, "update pod %s/%s", pod.Namespace, pod.Name)

	// Refresh the in-memory copy so subsequent GetPod returns the
	// latest spec / annotations.
	p.trackPod(pod, p.vmForPod(pod.Namespace, pod.Name))

	desire := bool(meta.ReadHibernateState(pod))
	v := p.vmForPod(pod.Namespace, pod.Name)

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
	p.notify(pod)
	return nil
}

// hibernate snapshots the VM into epoch under the well-known
// hibernate tag, then tears it down. The pod stays alive (vk-cocoon
// keeps the container in PodRunning) so K8s controllers do not
// recreate it; the operator detects the snapshot via
// epoch.GetManifest and marks the CocoonHibernation as Hibernated.
func (p *CocoonProvider) hibernate(ctx context.Context, pod *corev1.Pod, v *vm.VM) error {
	logger := providerLogger("hibernate")
	if err := p.Runtime.SnapshotSave(ctx, v.Name, v.ID); err != nil {
		return err
	}
	if p.Pusher != nil {
		if _, err := p.Pusher.PushSnapshot(ctx, v.Name, v.Name, meta.HibernateSnapshotTag, ""); err != nil {
			logger.Warnf(ctx, "push hibernation snapshot %s: %v", v.Name, err)
			return err
		}
	}
	if err := p.Runtime.Remove(ctx, v.ID); err != nil {
		return err
	}
	// Clear the runtime annotations: the VM no longer exists, but
	// the pod and the VMSpec stay so the wake path knows what to
	// restore.
	pod.Annotations[meta.AnnotationVMID] = ""
	pod.Annotations[meta.AnnotationIP] = ""
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
	if p.Puller != nil {
		if err := p.Puller.PullSnapshot(ctx, spec.VMName, meta.HibernateSnapshotTag, spec.VMName); err != nil {
			return err
		}
	}
	v, err := p.Runtime.Clone(ctx, vm.CloneOptions{
		From:     spec.VMName,
		To:       spec.VMName,
		Network:  spec.Network,
		Storage:  spec.Storage,
		NodeName: p.NodeName,
	})
	if err != nil {
		return err
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
	key := podKey(namespace, name)
	if v, ok := p.vmsByPod[key]; ok {
		delete(p.vmsByID, v.ID)
		delete(p.vmsByName, v.Name)
		delete(p.vmsByPod, key)
	}
}
