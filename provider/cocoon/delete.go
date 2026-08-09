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

func (p *Provider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("Provider.DeletePod")
	logger.Infof(ctx, "delete pod %s/%s", pod.Namespace, pod.Name)

	if err := p.backoffIfResuming(pod.Namespace, pod.Name); err != nil {
		return err
	}
	spec := meta.ParseVMSpec(pod)
	if isMacosSpec(spec) {
		return p.deleteMacosPod(ctx, pod, spec)
	}
	// A seat release keeps the local snapshot as the same-node warm-wake cache; resolveWakeSource still gates it on the :hibernate tag.
	keepSnapshots := meta.ReadKeepSnapshotOnDelete(pod)

	v := p.vmForPod(pod.Namespace, pod.Name)
	if v == nil {
		reason := "no_vm"
		if keepSnapshots {
			reason = "seat_release"
		} else {
			p.removeLocalSnapshots(ctx, spec.VMName)
		}
		p.forgetPod(pod.Namespace, pod.Name)
		metrics.PodLifecycleTotal.WithLabelValues("delete", "skipped", reason).Inc()
		return nil
	}

	if meta.ShouldSnapshotVM(spec, meta.RoleForPod(pod, spec.VMName)) && p.Pusher != nil && v.Name != "" {
		p.saveAndPushSnapshot(ctx, v.Name, v.ID, meta.DefaultSnapshotTag, spec.Image)
	}

	if err := p.Runtime.Remove(ctx, v.ID); err != nil {
		metrics.PodLifecycleTotal.WithLabelValues("delete", "failed", "").Inc()
		return fmt.Errorf("remove vm %s: %w", v.ID, err)
	}

	if !keepSnapshots {
		p.removeLocalSnapshots(ctx, v.Name)
	}

	p.forgetPod(pod.Namespace, pod.Name)
	pod.Status.Phase = corev1.PodSucceeded
	p.notify(pod)
	metrics.PodLifecycleTotal.WithLabelValues("delete", "ok", "").Inc()
	return nil
}

func (p *Provider) removeLocalSnapshots(ctx context.Context, vmName string) {
	if vmName == "" {
		return
	}
	// Synchronous on purpose: an immediate recreate must not race the rm.
	var wg sync.WaitGroup
	for _, name := range []string{vmName, forkSnapshotName(vmName)} {
		wg.Go(func() {
			p.removeSnapshotDetached(ctx, "Provider.DeletePod", name)
		})
	}
	wg.Wait()
}
