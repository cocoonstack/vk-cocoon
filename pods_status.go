package main

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GetPodStatus returns the latest status for a pod tracked by the
// provider. The status is derived from the in-memory VM record plus
// the probe manager's most recent reading.
func (p *CocoonProvider) GetPodStatus(_ context.Context, namespace, name string) (*corev1.PodStatus, error) {
	pod, err := p.GetPod(context.Background(), namespace, name)
	if err != nil {
		return nil, err
	}
	v := p.vmForPod(namespace, name)
	if v == nil {
		// VM gone (hibernated, or startup-reconcile orphan removed
		// it). Surface a Pending status with the original IP cleared.
		return &corev1.PodStatus{
			Phase:     corev1.PodPending,
			StartTime: pod.Status.StartTime,
		}, nil
	}

	ready := corev1.ConditionFalse
	if p.Probes != nil && p.Probes.Get(podKey(namespace, name)).Ready {
		ready = corev1.ConditionTrue
	}

	now := metav1.NewTime(metav1.Now().Time)
	status := &corev1.PodStatus{
		Phase:     corev1.PodRunning,
		PodIP:     v.IP,
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

// errNotImplemented is returned by stub kubelet operations vk-cocoon
// does not support yet.
var errNotImplemented = fmt.Errorf("not implemented by vk-cocoon")
