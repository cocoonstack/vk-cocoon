package cocoon

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/probes"
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
	lifecycleReady := meta.ReadLifecycleState(pod) == meta.LifecycleStateReady
	var probe probes.Result
	if p.Probes != nil {
		probe = p.Probes.Get(meta.PodKey(namespace, name))
	}
	if probe.Ready && lifecycleReady {
		ready = corev1.ConditionTrue
	}

	now := metav1.Now()
	status := &corev1.PodStatus{
		Phase:     corev1.PodRunning,
		PodIP:     podIP,
		StartTime: pod.Status.StartTime,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: ready, LastTransitionTime: now, Message: probe.Message},
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

// publishPodStatus writes the tracked pod.Status straight to the apiserver; NotifyPods only queues, so a direct patch could land first.
func (p *Provider) publishPodStatus(ctx context.Context, pod *corev1.Pod) error {
	if p.Clientset == nil {
		return nil
	}
	logger := log.WithFunc("Provider.publishPodStatus")
	p.mu.RLock()
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": pod.UID},
		"status":   pod.Status,
	})
	p.mu.RUnlock()
	if err != nil {
		logger.Errorf(ctx, err, "marshal status for %s/%s", pod.Namespace, pod.Name)
		return err
	}
	err = patchWithRetry(ctx, func() error {
		_, patchErr := p.Clientset.CoreV1().Pods(pod.Namespace).Patch(
			ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{}, "status")
		return patchErr
	})
	switch {
	case err == nil:
	case patchSuperseded(err):
		logger.Infof(ctx, "status publish for %s/%s superseded by a newer incarnation, skipping",
			pod.Namespace, pod.Name)
	default:
		logger.Errorf(ctx, err,
			"status publish failed after retries for %s/%s, lifecycle Ready remains pending", pod.Namespace, pod.Name)
	}
	return err
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
	currentCondition, currentFound := findCondition(current, conditionType)
	expectedCondition, expectedFound := findCondition(expected, conditionType)
	if currentFound != expectedFound || currentCondition.Status != expectedCondition.Status {
		return false
	}
	return conditionType != corev1.PodReady || currentCondition.Message == expectedCondition.Message
}

func findCondition(conditions []corev1.PodCondition, conditionType corev1.PodConditionType) (corev1.PodCondition, bool) {
	i := slices.IndexFunc(conditions, func(c corev1.PodCondition) bool { return c.Type == conditionType })
	if i < 0 {
		return corev1.PodCondition{}, false
	}
	return conditions[i], true
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
	i := slices.IndexFunc(statuses, func(s corev1.ContainerStatus) bool { return s.Name == name })
	if i < 0 {
		return corev1.ContainerStatus{}, false
	}
	return statuses[i], true
}
