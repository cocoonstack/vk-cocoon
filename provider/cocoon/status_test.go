package cocoon

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestReconcilePodStatusesRepublishesReadinessDrift(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	pod.Status = runningPodStatus(corev1.ConditionFalse)

	p := newTestProvider(t)
	p.Clientset = fake.NewSimpleClientset(pod)
	p.Probes.Set(meta.PodKey(pod.Namespace, pod.Name), probes.Result{Ready: true})
	p.trackPod(pod, &vm.VM{ID: "vmid", Name: "vk-ns-demo-0", IP: "192.0.2.10"})

	notified := make(chan *corev1.Pod, 1)
	p.notifyHook = func(updated *corev1.Pod) {
		notified <- updated
	}
	p.reconcilePodStatuses(t.Context())

	select {
	case updated := <-notified:
		if ready, _ := conditionStatus(updated.Status.Conditions, corev1.PodReady); ready != corev1.ConditionTrue {
			t.Fatalf("Ready = %s, want True", ready)
		}
		if !updated.Status.ContainerStatuses[0].Ready {
			t.Fatal("container Ready = false, want true")
		}
	default:
		t.Fatal("readiness drift was not republished")
	}
}

func TestReconcilePodStatusesSkipsMatchingStatus(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	pod.Status = runningPodStatus(corev1.ConditionTrue)

	p := newTestProvider(t)
	p.Clientset = fake.NewSimpleClientset(pod)
	p.Probes.Set(meta.PodKey(pod.Namespace, pod.Name), probes.Result{Ready: true})
	p.trackPod(pod, &vm.VM{ID: "vmid", Name: "vk-ns-demo-0", IP: "192.0.2.10"})

	notified := false
	p.notifyHook = func(*corev1.Pod) {
		notified = true
	}
	p.reconcilePodStatuses(t.Context())

	if notified {
		t.Fatal("matching status was republished")
	}
}

func runningPodStatus(ready corev1.ConditionStatus) corev1.PodStatus {
	now := metav1.Now()
	return corev1.PodStatus{
		Phase: corev1.PodRunning,
		PodIP: "192.0.2.10",
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: ready},
			{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
		},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  containerName,
			Ready: ready == corev1.ConditionTrue,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
				StartedAt: now,
			}},
		}},
	}
}
