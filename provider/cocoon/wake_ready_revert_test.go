package cocoon

import (
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cocoonstack/cocoon-common/meta"

	"github.com/cocoonstack/vk-cocoon/probes"
)

// newRevertFixture is newDropNICWakeFixture plus a fake apiserver holding the
// pod, with the lease already resolvable so waitForFreshIP returns on its first
// poll — the production shape when the guest DHCPs right after bring-up.
func newRevertFixture(t *testing.T) (*Provider, *corev1.Pod, *fake.Clientset) {
	t.Helper()
	p, pod, v := newDropNICWakeFixture(t, 2*time.Second, 1*time.Millisecond)
	p.Probes.Set(meta.PodKey("ns", "demo-0"), probes.Result{Ready: true})
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "demo-0"},
	})
	p.Clientset = client
	p.setVMIP("ns", "demo-0", v.ID, "172.20.1.228")
	t.Cleanup(func() { _ = v })
	return p, pod, client
}

// virtual-kubelet v1.12.0 PodController, the two lines that matter:
//
//	node/pod.go:336  kpod.lastPodStatusReceivedFromProvider = pod   // stores OUR pointer, no copy
//	node/pod.go:222  podFromProvider := kPod.lastPodStatus...DeepCopy()  // copies at DRAIN time
//	node/pod.go:251  podFromProvider.ResourceVersion = "0"; UpdateStatus(...)  // blind overwrite
//
// The drain running before markLifecycleStateForWake's status.Apply(pod) is a
// legal interleaving, and the one production hit at 11:40:05 took.
func TestWakeReadyAnnotationSurvivesFrameworkStatusPush(t *testing.T) {
	p, pod, client := newRevertFixture(t)

	var drained *corev1.Pod
	p.notifyHook = func(handed *corev1.Pod) {
		drained = handed.DeepCopy()
	}

	p.markReadyAfterIP(t.Context(), pod, p.vmForPod("ns", "demo-0"), true)

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

// The framework's own contract, node/pod.go:268-269:
//
//	// The pod must be DeepCopy'd prior to enqueuePodStatusUpdate.
//
// p.notify hands over the live pointer, and the worker DeepCopies it from
// another goroutine while markLifecycleStateForWake writes the same object
// under p.mu — a lock the framework never takes. Run under -race.
func TestWakeNotifyDoesNotShareMutablePod(t *testing.T) {
	p, pod, _ := newRevertFixture(t)

	var wg sync.WaitGroup
	p.notifyHook = func(handed *corev1.Pod) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				_ = handed.DeepCopy()
				time.Sleep(time.Microsecond)
			}
		}()
	}

	p.markReadyAfterIP(t.Context(), pod, p.vmForPod("ns", "demo-0"), true)
	wg.Wait()
}
