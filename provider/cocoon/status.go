package cocoon

import (
	"context"
	"encoding/json"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"
)

func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
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
				Name:  containerName,
				Ready: ready == corev1.ConditionTrue,
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{StartedAt: now},
				},
			},
		},
	}
	return status, nil
}

// publishPodStatus writes the tracked pod.Status straight to the apiserver;
// NotifyPods only queues, so a caller's next direct patch could land first.
func (p *Provider) publishPodStatus(ctx context.Context, pod *corev1.Pod) {
	if p.Clientset == nil {
		return
	}
	logger := log.WithFunc("Provider.publishPodStatus")
	p.mu.RLock()
	patch, err := json.Marshal(map[string]any{"status": pod.Status})
	p.mu.RUnlock()
	if err != nil {
		logger.Errorf(ctx, err, "marshal status for %s/%s", pod.Namespace, pod.Name)
		return
	}
	var lastErr error
	for range lifecyclePatchAttempts {
		_, lastErr = p.Clientset.CoreV1().Pods(pod.Namespace).Patch(
			ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{}, "status")
		if lastErr == nil {
			return
		}
		if !commonk8s.SleepCtx(ctx, lifecyclePatchInterval) {
			return
		}
	}
	logger.Errorf(ctx, lastErr,
		"status publish failed after retries for %s/%s, ready may precede podIP", pod.Namespace, pod.Name)
}

func podStatusMatches(current, expected corev1.PodStatus) bool {
	if current.Phase != expected.Phase || current.PodIP != expected.PodIP {
		return false
	}
	if !conditionMatches(current.Conditions, expected.Conditions, corev1.PodReady) ||
		!conditionMatches(current.Conditions, expected.Conditions, corev1.PodInitialized) {
		return false
	}
	return containerStatusMatches(current.ContainerStatuses, expected.ContainerStatuses)
}

func conditionMatches(current, expected []corev1.PodCondition, conditionType corev1.PodConditionType) bool {
	currentStatus, currentFound := conditionStatus(current, conditionType)
	expectedStatus, expectedFound := conditionStatus(expected, conditionType)
	return currentFound == expectedFound && currentStatus == expectedStatus
}

func conditionStatus(conditions []corev1.PodCondition, conditionType corev1.PodConditionType) (corev1.ConditionStatus, bool) {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status, true
		}
	}
	return "", false
}

func containerStatusMatches(current, expected []corev1.ContainerStatus) bool {
	currentStatus, currentFound := findContainerStatus(current, containerName)
	expectedStatus, expectedFound := findContainerStatus(expected, containerName)
	if currentFound != expectedFound {
		return false
	}
	if !currentFound {
		return true
	}
	return currentStatus.Ready == expectedStatus.Ready &&
		(currentStatus.State.Running != nil) == (expectedStatus.State.Running != nil) &&
		(currentStatus.State.Waiting != nil) == (expectedStatus.State.Waiting != nil) &&
		(currentStatus.State.Terminated != nil) == (expectedStatus.State.Terminated != nil)
}

func findContainerStatus(statuses []corev1.ContainerStatus, name string) (corev1.ContainerStatus, bool) {
	for _, status := range statuses {
		if status.Name == name {
			return status, true
		}
	}
	return corev1.ContainerStatus{}, false
}
