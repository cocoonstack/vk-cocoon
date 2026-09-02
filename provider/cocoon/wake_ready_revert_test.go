package cocoon

import (
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cocoonstack/cocoon-common/meta"
)

func TestWakeReadyAnnotationSurvivesFrameworkStatusPush(t *testing.T) {
	p, pod, v, client := newWakeClientsetFixture(t)
	p.setVMIP("ns", "demo-0", v.ID, "172.20.1.228")

	var drained *corev1.Pod
	p.notifyHook = func(handed *corev1.Pod) {
		drained = handed.DeepCopy()
	}

	p.markReadyAfterIP(t.Context(), pod, meta.ParseVMSpec(pod), v, true)

	if drained == nil {
		t.Fatal("notify was never called; the framework push is not being modelled")
	}
	t.Logf("annotation in the drained copy = %q", meta.ReadLifecycleState(drained))

	drained.ResourceVersion = "0"
	if _, err := client.CoreV1().Pods("ns").UpdateStatus(t.Context(), drained, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("framework status push: %v", err)
	}

	got, err := client.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if state := meta.ReadLifecycleState(got); state != meta.LifecycleStateReady {
		t.Errorf("apiserver lifecycle-state = %q after the framework status push, want %q",
			state, meta.LifecycleStateReady)
	}
}

func TestWakeNotifyDoesNotShareMutablePod(t *testing.T) {
	p, pod, v, _ := newWakeClientsetFixture(t)
	p.setVMIP("ns", "demo-0", v.ID, "172.20.1.228")

	var wg sync.WaitGroup
	p.notifyHook = func(handed *corev1.Pod) {
		wg.Go(func() {
			for range 200 {
				_ = handed.DeepCopy()
				time.Sleep(time.Microsecond)
			}
		})
	}

	p.markReadyAfterIP(t.Context(), pod, meta.ParseVMSpec(pod), v, true)
	wg.Wait()
}
