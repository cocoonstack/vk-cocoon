package cocoon

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestReconcilePodStatusesRepublishesReadinessDrift(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	meta.LifecycleStatus{State: meta.LifecycleStateReady, ObservedGeneration: 1}.Apply(pod)
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
		if ready, _ := findCondition(updated.Status.Conditions, corev1.PodReady); ready.Status != corev1.ConditionTrue {
			t.Fatalf("Ready = %s, want True", ready.Status)
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
	meta.LifecycleStatus{State: meta.LifecycleStateReady, ObservedGeneration: 1}.Apply(pod)
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

func TestReconcilePodStatusesRepairsIPAnnotationWithoutReadinessTransition(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	meta.LifecycleStatus{State: meta.LifecycleStateReady, ObservedGeneration: 1}.Apply(pod)
	pod.Annotations[meta.AnnotationIP] = "172.20.0.42"
	pod.Status = runningPodStatus(corev1.ConditionTrue)
	pod.Status.PodIP = "172.20.0.88"

	p := newTestProvider(t)
	client := fake.NewSimpleClientset(pod)
	p.Clientset = client
	p.LeaseParser = newLeaseParser(t, "aa:bb:cc:dd:ee:ff", "172.20.0.88")
	p.Probes.Set(meta.PodKey(pod.Namespace, pod.Name), probes.Result{Ready: true})
	p.trackPod(pod, &vm.VM{
		ID: "vmid", Name: "vk-ns-demo-0", MAC: "aa:bb:cc:dd:ee:ff", IP: "172.20.0.42",
	})

	p.reconcilePodStatuses(t.Context())
	p.reconcilePodStatuses(t.Context())

	got, err := p.Clientset.CoreV1().Pods(pod.Namespace).Get(t.Context(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if ip := got.Annotations[meta.AnnotationIP]; ip != "172.20.0.88" {
		t.Fatalf("IP annotation = %q, want rebound 172.20.0.88", ip)
	}
	patches := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			patches++
		}
	}
	if patches != 1 {
		t.Fatalf("annotation patches = %d after repeated reconciliation, want 1", patches)
	}
}

func TestReconcilePodStatusesSkipsRecreatedPodIncarnation(t *testing.T) {
	podA := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	podA.UID = "a"
	meta.LifecycleStatus{State: meta.LifecycleStateReady, ObservedGeneration: 1}.Apply(podA)
	podA.Status = runningPodStatus(corev1.ConditionFalse)

	podB := podA.DeepCopy()
	podB.UID = "b"
	podB.Status = corev1.PodStatus{Phase: corev1.PodPending}

	p := newTestProvider(t)
	client := fake.NewSimpleClientset(podB)
	p.Clientset = client
	p.Probes.Set(meta.PodKey(podA.Namespace, podA.Name), probes.Result{Ready: true})
	p.trackPod(podA, &vm.VM{ID: "vmid", Name: "vk-ns-demo-0", IP: "192.0.2.10"})

	notified := make(chan *corev1.Pod, 1)
	p.notifyHook = func(updated *corev1.Pod) {
		notified <- updated
	}
	p.reconcilePodStatuses(t.Context())

	select {
	case updated := <-notified:
		t.Errorf("published superseded incarnation status onto UID %q", updated.UID)
	default:
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			t.Errorf("patched runtime endpoints onto the recreated incarnation: %v", action.GetResource())
		}
	}
}

func TestReconcilePodStatusesRepublishesProbeMessageDrift(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	meta.LifecycleStatus{State: meta.LifecycleStateReady, ObservedGeneration: 1}.Apply(pod)
	pod.Status = runningPodStatus(corev1.ConditionTrue)

	p := newTestProvider(t)
	p.Clientset = fake.NewSimpleClientset(pod)
	p.Probes.Set(meta.PodKey(pod.Namespace, pod.Name), probes.Result{Ready: true, Message: "tcp ok"})
	p.trackPod(pod, &vm.VM{ID: "vmid", Name: "vk-ns-demo-0", IP: "192.0.2.10"})

	notified := make(chan *corev1.Pod, 1)
	p.notifyHook = func(updated *corev1.Pod) {
		notified <- updated
	}
	p.reconcilePodStatuses(t.Context())

	select {
	case updated := <-notified:
		if ready, _ := findCondition(updated.Status.Conditions, corev1.PodReady); ready.Message != "tcp ok" {
			t.Fatalf("Ready message = %q, want %q", ready.Message, "tcp ok")
		}
	default:
		t.Fatal("probe message drift was not republished")
	}
}

func TestReconcilePodStatusesSkipsSupersededIncarnationBehindStaleLister(t *testing.T) {
	podA := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	podA.UID = "a"
	podA.Annotations[meta.AnnotationIP] = "192.0.2.10"
	meta.LifecycleStatus{State: meta.LifecycleStateReady, ObservedGeneration: 1}.Apply(podA)
	podA.Status = runningPodStatus(corev1.ConditionFalse)

	podB := podA.DeepCopy()
	podB.UID = "b"
	podB.Annotations[meta.AnnotationIP] = "192.0.2.99"
	podB.Status = corev1.PodStatus{Phase: corev1.PodPending}

	p := newTestProvider(t)
	client := fake.NewSimpleClientset(podB)
	client.PrependReactor("patch", "pods", rejectMismatchedUID(string(podB.UID)))
	p.Clientset = client
	p.Pods = stalePodLister(t, podA)
	p.Probes.Set(meta.PodKey(podA.Namespace, podA.Name), probes.Result{Ready: true})
	p.trackPod(podA, &vm.VM{ID: "vmid", Name: "vk-ns-demo-0", IP: "192.0.2.55"})

	notified := make(chan *corev1.Pod, 1)
	p.notifyHook = func(updated *corev1.Pod) {
		notified <- updated
	}
	p.reconcilePodStatuses(t.Context())

	live, err := client.CoreV1().Pods(podB.Namespace).Get(t.Context(), podB.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if ip := live.Annotations[meta.AnnotationIP]; ip != "192.0.2.99" {
		t.Errorf("successor IP annotation = %q, want its own 192.0.2.99", ip)
	}
	select {
	case updated := <-notified:
		t.Errorf("enqueued status of superseded incarnation %q", updated.UID)
	default:
	}
}

func TestReconcilePodStatusesDropsNotifyWhenTrackingFlipsDuringPatch(t *testing.T) {
	podA := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	podA.UID = "a"
	podA.Annotations[meta.AnnotationIP] = "192.0.2.10"
	meta.LifecycleStatus{State: meta.LifecycleStateReady, ObservedGeneration: 1}.Apply(podA)
	podA.Status = runningPodStatus(corev1.ConditionFalse)

	podB := podA.DeepCopy()
	podB.UID = "b"

	p := newTestProvider(t)
	client := fake.NewSimpleClientset(podA)
	client.PrependReactor("patch", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		p.trackPod(podB, nil)
		return false, nil, nil
	})
	p.Clientset = client
	p.Pods = stalePodLister(t, podA)
	p.Probes.Set(meta.PodKey(podA.Namespace, podA.Name), probes.Result{Ready: true})
	p.trackPod(podA, &vm.VM{ID: "vmid", Name: "vk-ns-demo-0", IP: "192.0.2.55"})

	notified := make(chan *corev1.Pod, 1)
	p.notifyHook = func(updated *corev1.Pod) {
		notified <- updated
	}
	p.reconcilePodStatuses(t.Context())

	select {
	case updated := <-notified:
		t.Errorf("enqueued status of incarnation %q after tracking moved on", updated.UID)
	default:
	}
}

func TestGetPodStatusGatesProbeReadyUntilLifecycleReady(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "clone", OS: "windows"})
	meta.LifecycleStatus{State: meta.LifecycleStateCreating, ObservedGeneration: 1}.Apply(pod)

	p := newTestProvider(t)
	p.Probes.Set(meta.PodKey(pod.Namespace, pod.Name), probes.Result{Ready: true})
	p.trackPod(pod, &vm.VM{ID: "vmid", Name: "vk-ns-demo-0", IP: "192.0.2.10"})

	status, err := p.GetPodStatus(t.Context(), pod.Namespace, pod.Name)
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if ready, _ := findCondition(status.Conditions, corev1.PodReady); ready.Status != corev1.ConditionFalse {
		t.Fatalf("Ready = %s, want False while lifecycle is creating", ready.Status)
	}
	if status.ContainerStatuses[0].Ready {
		t.Fatal("container Ready = true while lifecycle is creating")
	}
}

func stalePodLister(t *testing.T, pods ...*corev1.Pod) corev1listers.PodLister {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})
	for _, pod := range pods {
		if err := indexer.Add(pod); err != nil {
			t.Fatalf("seed lister with %s/%s: %v", pod.Namespace, pod.Name, err)
		}
	}
	return corev1listers.NewPodLister(indexer)
}

func rejectMismatchedUID(uid string) k8stesting.ReactionFunc {
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(k8stesting.PatchAction)
		var body map[string]map[string]any
		if err := json.Unmarshal(patchAction.GetPatch(), &body); err != nil {
			return true, nil, err
		}
		patched, _ := body["metadata"]["uid"].(string)
		if patched == "" || patched == uid {
			return false, nil, nil
		}
		return true, nil, apierrors.NewInvalid(
			corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), patchAction.GetName(), nil)
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
