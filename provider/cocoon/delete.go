package cocoon

import (
	"context"
	"fmt"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
)

// DeletePod removes a pod, optionally snapshotting the VM first.
func (p *Provider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("Provider.DeletePod")
	logger.Infof(ctx, "delete pod %s/%s", pod.Namespace, pod.Name)

	v := p.vmForPod(pod.Namespace, pod.Name)
	if v == nil {
		// No VM to tear down; forget and succeed.
		p.forgetPod(pod.Namespace, pod.Name)
		metrics.PodLifecycleTotal.WithLabelValues("delete", "no_vm").Inc()
		return nil
	}

	spec := meta.ParseVMSpec(pod)

	if meta.ShouldSnapshotVM(spec) && p.Pusher != nil && v.Name != "" {
		p.saveAndPushSnapshot(ctx, v.Name, v.ID, meta.DefaultSnapshotTag, spec.Image)
	}

	if err := p.Runtime.Remove(ctx, v.ID); err != nil {
		metrics.PodLifecycleTotal.WithLabelValues("delete", "failed").Inc()
		return fmt.Errorf("remove vm %s: %w", v.ID, err)
	}

	p.forgetPod(pod.Namespace, pod.Name)
	pod.Status.Phase = corev1.PodSucceeded
	p.notify(pod)
	metrics.PodLifecycleTotal.WithLabelValues("delete", "ok").Inc()
	return nil
}
