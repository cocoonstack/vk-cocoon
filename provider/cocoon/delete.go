package cocoon

import (
	"context"
	"fmt"
	"sync"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
)

// DeletePod removes a pod, optionally snapshotting the VM first.
func (p *Provider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("Provider.DeletePod")
	logger.Infof(ctx, "delete pod %s/%s", pod.Namespace, pod.Name)

	spec := meta.ParseVMSpec(pod)

	v := p.vmForPod(pod.Namespace, pod.Name)
	if v == nil {
		p.removeLocalSnapshots(spec.VMName)
		p.forgetPod(pod.Namespace, pod.Name)
		metrics.PodLifecycleTotal.WithLabelValues("delete", "skipped", "no_vm").Inc()
		return nil
	}

	if meta.ShouldSnapshotVM(spec, meta.RoleForPod(pod, spec.VMName)) && p.Pusher != nil && v.Name != "" {
		p.saveAndPushSnapshot(ctx, v.Name, v.ID, meta.DefaultSnapshotTag, spec.Image)
	}

	if err := p.Runtime.Remove(ctx, v.ID); err != nil {
		metrics.PodLifecycleTotal.WithLabelValues("delete", "failed", "").Inc()
		return fmt.Errorf("remove vm %s: %w", v.ID, err)
	}

	p.removeLocalSnapshots(v.Name)

	p.forgetPod(pod.Namespace, pod.Name)
	pod.Status.Phase = corev1.PodSucceeded
	p.notify(pod)
	metrics.PodLifecycleTotal.WithLabelValues("delete", "ok", "").Inc()
	return nil
}

// removeLocalSnapshots drops the clone source and its fork snapshot so a later restore cannot prefer stale local state over the registry tag.
func (p *Provider) removeLocalSnapshots(vmName string) {
	if vmName == "" {
		return
	}
	// Synchronous on purpose: an immediate recreate must not race the rm.
	var wg sync.WaitGroup
	for _, name := range []string{vmName, forkSnapshotName(vmName)} {
		wg.Go(func() {
			p.removeSnapshotDetached("Provider.DeletePod", name)
		})
	}
	wg.Wait()
}
