package main

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cocoonstack/cocoon-common/meta"
)

// GetPodStatus derives status from the VM record and latest probe result.
func (p *CocoonProvider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	v := p.vmForPod(namespace, name)
	if v == nil {
		// VM gone (hibernated or removed).
		return &corev1.PodStatus{
			Phase:     corev1.PodPending,
			StartTime: pod.Status.StartTime,
		}, nil
	}
	podIP := p.resolveVMIP(namespace, name, v)

	ready := corev1.ConditionFalse
	if p.Probes != nil && p.Probes.Get(meta.PodKey(namespace, name)).Ready {
		ready = corev1.ConditionTrue
	}

	now := metav1.Now()
	status := &corev1.PodStatus{
		Phase:     corev1.PodRunning,
		PodIP:     podIP,
		StartTime: pod.Status.StartTime,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: ready, LastTransitionTime: now},
			{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: now},
		},
		ContainerStatuses: []corev1.ContainerStatus{
			{
				Name:  "agent",
				Ready: ready == corev1.ConditionTrue,
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{StartedAt: now},
				},
			},
		},
	}
	return status, nil
}
