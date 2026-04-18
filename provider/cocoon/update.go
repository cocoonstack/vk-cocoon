package cocoon

import (
	"context"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	// hibernateImportSuffix avoids name collision between the import target
	// and the live VM that the subsequent Clone produces.
	hibernateImportSuffix = "-hibernate-import"
)

// UpdatePod handles hibernate/wake transitions. Other spec changes are ignored.
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
			return err
		}
		metrics.PodLifecycleTotal.WithLabelValues("update", "hibernated").Inc()
	case !wantHibernate && v == nil:
		// Wake: recreate from the hibernation snapshot.
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

// hibernate snapshots the VM to epoch then tears it down. Order is
// Save -> Push -> Remove. If Remove fails, the pushed tag is rolled back
// so the operator does not observe Hibernated while the VM is still running.
func (p *Provider) hibernate(ctx context.Context, pod *corev1.Pod, v *vm.VM) error {
	logger := log.WithFunc("Provider.hibernate")
	saveStart := time.Now()
	if err := p.Runtime.SnapshotSave(ctx, v.Name, v.ID); err != nil {
		return fmt.Errorf("snapshot save %s: %w", v.Name, err)
	}
	metrics.SnapshotSaveDuration.Observe(time.Since(saveStart).Seconds())
	if p.Pusher != nil {
		pushStart := time.Now()
		if _, err := p.Pusher.PushSnapshot(ctx, v.Name, v.Name, meta.HibernateSnapshotTag, ""); err != nil {
			return fmt.Errorf("push hibernation snapshot %s: %w", v.Name, err)
		}
		metrics.SnapshotPushDuration.Observe(time.Since(pushStart).Seconds())
	}
	if err := p.Runtime.Remove(ctx, v.ID); err != nil {
		if p.Registry != nil {
			// Roll back the hibernate tag.
			if delErr := p.Registry.DeleteManifest(ctx, v.Name, meta.HibernateSnapshotTag); delErr != nil {
				logger.Errorf(ctx, delErr, "rollback hibernate push after remove failed for %s", v.Name)
			}
		}
		return fmt.Errorf("remove vm %s: %w", v.ID, err)
	}
	// Clear runtime annotations; the pod stays so wake knows what to restore.
	if pod.Annotations != nil {
		delete(pod.Annotations, meta.AnnotationVMID)
		delete(pod.Annotations, meta.AnnotationIP)
	}
	p.forgetVMOnly(pod.Namespace, pod.Name)
	return nil
}

// wake restores the VM from the hibernation snapshot.
func (p *Provider) wake(ctx context.Context, pod *corev1.Pod) error {
	spec := meta.ParseVMSpec(pod)
	if spec.VMName == "" {
		return nil
	}
	if p.Puller == nil {
		// Cannot import without a puller.
		return fmt.Errorf("wake %s: no snapshot puller configured", spec.VMName)
	}
	cpu, memory := vmResourceOverrides(pod)
	importName := spec.VMName + hibernateImportSuffix
	if err := p.Puller.PullSnapshot(ctx, spec.VMName, meta.HibernateSnapshotTag, importName); err != nil {
		return fmt.Errorf("pull hibernation snapshot %s: %w", spec.VMName, err)
	}
	v, err := p.Runtime.Clone(ctx, vm.CloneOptions{
		From:       importName,
		To:         spec.VMName,
		CPU:        cpu,
		Memory:     memory,
		Network:    spec.Network,
		Storage:    spec.Storage,
		Backend:    spec.Backend,
		NoDirectIO: spec.NoDirectIO,
	})
	if err != nil {
		return fmt.Errorf("clone vm %s from %s: %w", spec.VMName, importName, err)
	}
	p.emitPostCloneHint(ctx, pod, spec, v, "") // wake has no snapshot source metadata
	p.applyRuntime(ctx, pod, v)
	p.trackPod(pod, v)
	// Hibernate tag cleanup is the operator's responsibility (reconcileWake).
	return nil
}

// forgetVMOnly clears the VM record but keeps the pod (used by hibernate).
func (p *Provider) forgetVMOnly(namespace, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropVMLocked(meta.PodKey(namespace, name))
}
