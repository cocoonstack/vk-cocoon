package cocoon

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
)

func (p *Provider) assertPodSnapshotCompatibility(pod *corev1.Pod) error {
	// Terminal pods never resume a snapshot; a leftover Succeeded Job must not block node registration.
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return nil
	}
	requested := pod.Spec.NodeSelector[meta.LabelSnapshotCompatibilityClass]
	if requested == "" {
		return nil
	}
	if p.SnapshotCompatibilityClass == "" {
		return fmt.Errorf("pod %s/%s requires snapshot compatibility class %q but node %s is unclassified",
			pod.Namespace, pod.Name, requested, p.NodeName)
	}
	if requested != p.SnapshotCompatibilityClass {
		return fmt.Errorf("pod %s/%s requires snapshot compatibility class %q but node %s provides %q",
			pod.Namespace, pod.Name, requested, p.NodeName, p.SnapshotCompatibilityClass)
	}
	return nil
}
