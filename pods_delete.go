package main

import (
	"context"
	"fmt"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
)

// DeletePod is the virtual-kubelet entry point for pod removal. It
// optionally snapshots the VM into epoch (driven by the
// SnapshotPolicy annotation), tells the cocoon runtime to destroy
// the VM, and forgets the pod from the in-memory tables.
func (p *CocoonProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("CocoonProvider.DeletePod")
	logger.Infof(ctx, "delete pod %s/%s", pod.Namespace, pod.Name)

	v := p.vmForPod(pod.Namespace, pod.Name)
	if v == nil {
		// Forget the pod and return success so the controller stops
		// retrying — there is no VM to tear down.
		p.forgetPod(pod.Namespace, pod.Name)
		metrics.PodLifecycleTotal.WithLabelValues("delete", "no_vm").Inc()
		return nil
	}

	spec := meta.ParseVMSpec(pod)

	if meta.ShouldSnapshotVM(spec) && p.Pusher != nil && v.Name != "" {
		if err := p.Runtime.SnapshotSave(ctx, v.Name, v.ID); err != nil {
			logger.Warnf(ctx, "snapshot save %s: %v", v.Name, err)
			metrics.SnapshotPushTotal.WithLabelValues("failed").Inc()
		} else if _, err := p.Pusher.PushSnapshot(ctx, v.Name, "", meta.DefaultSnapshotTag, spec.Image); err != nil {
			logger.Warnf(ctx, "push snapshot %s: %v", v.Name, err)
			metrics.SnapshotPushTotal.WithLabelValues("failed").Inc()
		} else {
			metrics.SnapshotPushTotal.WithLabelValues("ok").Inc()
		}
	}

	if err := p.Runtime.Remove(ctx, v.ID); err != nil {
		metrics.PodLifecycleTotal.WithLabelValues("delete", "failed").Inc()
		return fmt.Errorf("remove vm %s: %w", v.ID, err)
	}

	p.forgetPod(pod.Namespace, pod.Name)
	if p.Probes != nil {
		p.Probes.Forget(meta.PodKey(pod.Namespace, pod.Name))
	}
	pod.Status.Phase = corev1.PodSucceeded
	p.notify(pod)
	metrics.PodLifecycleTotal.WithLabelValues("delete", "ok").Inc()
	metrics.VMTableSize.Dec()
	return nil
}
