package cocoon

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestMarkLifecycleStateWritesAtomicTriple(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "demo-0", Namespace: "ns",
		Annotations: map[string]string{meta.AnnotationCocoonSetGeneration: "5"},
	}}
	cs := fake.NewSimpleClientset(pod)
	p := newTestProvider(t)
	p.Clientset = cs

	p.markLifecycleState(t.Context(), pod, meta.LifecycleStateHibernating, "")

	updated, err := cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got := updated.Annotations[meta.AnnotationLifecycleState]; got != "hibernating" {
		t.Errorf("state = %q, want hibernating", got)
	}
	if got := updated.Annotations[meta.AnnotationLifecycleObservedGeneration]; got != "5" {
		t.Errorf("observed-generation = %q, want 5", got)
	}
	if _, ok := updated.Annotations[meta.AnnotationLifecycleStateMessage]; ok {
		t.Errorf("message must be unset for non-failed state, got %q",
			updated.Annotations[meta.AnnotationLifecycleStateMessage])
	}
}

func TestMarkLifecycleStateClearsMessageOnTerminalSuccess(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "demo-0", Namespace: "ns",
		Annotations: map[string]string{
			meta.AnnotationLifecycleStateMessage: "stale failure reason",
		},
	}}
	cs := fake.NewSimpleClientset(pod)
	p := newTestProvider(t)
	p.Clientset = cs

	p.markLifecycleState(t.Context(), pod, meta.LifecycleStateReady, "")

	updated, _ := cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if msg, ok := updated.Annotations[meta.AnnotationLifecycleStateMessage]; ok {
		t.Errorf("ready state must clear stale message, got %q", msg)
	}
}

func TestMarkLifecycleStateRecordsMessageOnFailed(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	cs := fake.NewSimpleClientset(pod)
	p := newTestProvider(t)
	p.Clientset = cs

	p.markLifecycleState(t.Context(), pod, meta.LifecycleStateFailed, "boot exploded")

	updated, _ := cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if got := updated.Annotations[meta.AnnotationLifecycleState]; got != "failed" {
		t.Errorf("state = %q", got)
	}
	if got := updated.Annotations[meta.AnnotationLifecycleStateMessage]; got != "boot exploded" {
		t.Errorf("message = %q", got)
	}
}

func TestReconcileSkipsPodWithoutLifecycleState(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	cs := fake.NewSimpleClientset(pod)
	patches := 0
	cs.PrependReactor("patch", "pods", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		patches++
		return false, nil, nil
	})
	p := newTestProvider(t)
	p.Clientset = cs
	p.trackPod(pod, nil)

	p.reconcileAllLifecycle(t.Context())

	if patches != 0 {
		t.Errorf("reconcile must not patch pods without lifecycle annotation, patches=%d", patches)
	}
}

func TestReconcileSkipsWhenNoDrift(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	cs := fake.NewSimpleClientset(pod)
	p := newTestProvider(t)
	p.Clientset = cs
	p.trackPod(pod, nil)

	p.markLifecycleState(t.Context(), pod, meta.LifecycleStateReady, "")

	patches := 0
	cs.PrependReactor("patch", "pods", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		patches++
		return false, nil, nil
	})
	p.reconcileAllLifecycle(t.Context())
	if patches != 0 {
		t.Errorf("reconcile should no-op when in-memory == flushed, got %d patches", patches)
	}
}

func TestReconcileFixesDriftWhenFlushedIsStale(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	cs := fake.NewSimpleClientset(pod)
	p := newTestProvider(t)
	p.Clientset = cs

	key := meta.PodKey("ns", "demo-0")
	intent := meta.LifecycleStatus{State: meta.LifecycleStateHibernated, ObservedGeneration: 4}
	p.lifecycleIntent[key] = lifecycleEntry{status: intent}
	stale := meta.LifecycleStatus{State: meta.LifecycleStateCreating, ObservedGeneration: 3}
	p.recordLifecycleFlushed(key, "", stale.Snapshot())

	p.reconcileAllLifecycle(t.Context())

	updated, _ := cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if got := updated.Annotations[meta.AnnotationLifecycleState]; got != "hibernated" {
		t.Errorf("apiserver state after reconcile = %q, want hibernated", got)
	}
	if got := p.lifecycleIntent[key].flushed; got != intent.Snapshot() {
		t.Errorf("flushed snapshot after reconcile = %q, want %q", got, intent.Snapshot())
	}
}

func TestReadyPublicationRetriesStatusBeforeLifecycle(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "demo-0", Namespace: "ns",
		Annotations: map[string]string{meta.AnnotationLifecycleState: string(meta.LifecycleStateCreating)},
	}}
	cs := fake.NewSimpleClientset(pod.DeepCopy())
	failStatus := true
	statusPublished := false
	cs.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch := action.(k8stesting.PatchAction)
		if patch.GetSubresource() == "status" {
			if failStatus {
				return true, nil, errors.New("status patch failed")
			}
			statusPublished = true
			return false, nil, nil
		}
		if strings.Contains(string(patch.GetPatch()), string(meta.LifecycleStateReady)) && !statusPublished {
			t.Fatal("ready lifecycle annotation published before pod status")
		}
		return false, nil, nil
	})

	p := newTestProvider(t)
	p.Clientset = cs
	p.Probes.Set(meta.PodKey("ns", "demo-0"), probes.Result{Ready: true})
	p.trackPod(pod, &vm.VM{ID: "vmid", Name: "demo", IP: "192.0.2.10"})
	readyNotifications := 0
	p.notifyHook = func(notified *corev1.Pod) {
		if ready, _ := findCondition(notified.Status.Conditions, corev1.PodReady); ready.Status == corev1.ConditionTrue {
			readyNotifications++
		}
	}

	p.markReadyPublished(t.Context(), pod)

	updated, _ := cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if got := updated.Annotations[meta.AnnotationLifecycleState]; got != string(meta.LifecycleStateCreating) {
		t.Fatalf("lifecycle state = %q after failed status publish, want creating", got)
	}
	if readyNotifications != 0 {
		t.Fatalf("Ready notification published after failed status write: %d", readyNotifications)
	}

	failStatus = false
	p.reconcileAllLifecycle(t.Context())

	updated, _ = cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if got := updated.Annotations[meta.AnnotationLifecycleState]; got != string(meta.LifecycleStateReady) {
		t.Fatalf("lifecycle state = %q after reconciliation, want ready", got)
	}
	if !statusPublished {
		t.Fatal("ready status was not republished before lifecycle reconciliation")
	}
	if readyNotifications != 1 {
		t.Fatalf("Ready notifications after successful reconciliation = %d, want 1", readyNotifications)
	}
}

func TestReadyPublicationNotifiesWhenLifecycleTransitionIsRejected(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	failed := meta.LifecycleStatus{State: meta.LifecycleStateFailed}
	failed.Apply(pod)

	p := newTestProvider(t)
	p.Clientset = fake.NewSimpleClientset(pod.DeepCopy())
	p.Probes.Set(meta.PodKey("ns", "demo-0"), probes.Result{Ready: true})
	p.trackPod(pod, &vm.VM{ID: "vmid", Name: "demo", IP: "192.0.2.10"})
	p.lifecycleIntent[meta.PodKey("ns", "demo-0")] = lifecycleEntry{status: failed}

	notified := false
	p.notifyHook = func(updated *corev1.Pod) {
		notified = true
		if ready, _ := findCondition(updated.Status.Conditions, corev1.PodReady); ready.Status != corev1.ConditionFalse {
			t.Fatalf("Ready = %s, want False while lifecycle remains failed", ready.Status)
		}
	}

	p.markReadyPublished(t.Context(), pod)
	if !notified {
		t.Fatal("status was not notified after the rejected lifecycle transition")
	}
}

func TestReconcileDropsReadyIntentForDeletedPod(t *testing.T) {
	p := newTestProvider(t)
	p.Clientset = fake.NewSimpleClientset()
	key := meta.PodKey("ns", "gone-0")
	p.mu.Lock()
	p.lifecycleIntent[key] = lifecycleEntry{status: meta.LifecycleStatus{State: meta.LifecycleStateReady, ObservedGeneration: 1}}
	p.mu.Unlock()

	p.reconcileAllLifecycle(t.Context())

	p.mu.RLock()
	_, ok := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if ok {
		t.Fatal("ready intent for a deleted, untracked pod must be dropped, not retried forever")
	}
}

func TestReconcileIgnoresStalePodAnnotations(t *testing.T) {
	t.Parallel()

	stalePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "demo-0", Namespace: "ns",
		Annotations: map[string]string{
			meta.AnnotationLifecycleState:              "ready",
			meta.AnnotationLifecycleObservedGeneration: "3",
		},
	}}
	cs := fake.NewSimpleClientset(stalePod)
	p := newTestProvider(t)
	p.Clientset = cs
	p.trackPod(stalePod, nil)

	key := meta.PodKey("ns", "demo-0")
	intent := meta.LifecycleStatus{State: meta.LifecycleStateHibernating, ObservedGeneration: 4}
	p.lifecycleIntent[key] = lifecycleEntry{status: intent}

	p.reconcileAllLifecycle(t.Context())

	updated, _ := cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if got := updated.Annotations[meta.AnnotationLifecycleState]; got != "hibernating" {
		t.Errorf("reconcile must publish intent (hibernating) not stale pod annotation, got %q", got)
	}
	if got := updated.Annotations[meta.AnnotationLifecycleObservedGeneration]; got != "4" {
		t.Errorf("reconcile must publish intent obs-gen=4, got %q", got)
	}
}

func TestMarkLifecycleStateUsesLatestTrackedGen(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "demo-0", Namespace: "ns",
		Annotations: map[string]string{meta.AnnotationCocoonSetGeneration: "5"},
	}}
	cs := fake.NewSimpleClientset(pod)
	p := newTestProvider(t)
	p.Clientset = cs
	p.trackPod(pod, nil)

	stale := pod.DeepCopy()
	stale.Annotations[meta.AnnotationCocoonSetGeneration] = "3"
	p.markLifecycleState(t.Context(), stale, meta.LifecycleStateReady, "")

	key := meta.PodKey("ns", "demo-0")
	p.mu.RLock()
	got := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if got.status.ObservedGeneration != 5 {
		t.Errorf("intent gen = %d, want 5 (tracked pod's gen)", got.status.ObservedGeneration)
	}
}

func TestUpdatePodNoopRepublishesOnGenerationBump(t *testing.T) {
	t.Parallel()

	pod := newPodWithSpec(meta.VMSpec{VMName: "demo", Mode: "run"})
	pod.Annotations[meta.AnnotationCocoonSetGeneration] = "1"
	cs := fake.NewSimpleClientset(pod)
	p := newTestProvider(t)
	p.Clientset = cs
	p.trackPod(pod, &vm.VM{ID: "vmid", Name: "demo"})
	p.markLifecycleState(t.Context(), pod, meta.LifecycleStateReady, "")

	bumped := pod.DeepCopy()
	bumped.Annotations[meta.AnnotationCocoonSetGeneration] = "2"
	if err := p.UpdatePod(t.Context(), bumped); err != nil {
		t.Fatalf("UpdatePod: %v", err)
	}

	updated, _ := cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if got := updated.Annotations[meta.AnnotationLifecycleObservedGeneration]; got != "2" {
		t.Errorf("observed-generation = %q, want 2", got)
	}
	if got := updated.Annotations[meta.AnnotationLifecycleState]; got != "ready" {
		t.Errorf("state = %q, want ready (must not change)", got)
	}
}

func TestUpdatePodNoopSkipsRepublishWhenGenUnchanged(t *testing.T) {
	t.Parallel()

	pod := newPodWithSpec(meta.VMSpec{VMName: "demo", Mode: "run"})
	pod.Annotations[meta.AnnotationCocoonSetGeneration] = "1"
	cs := fake.NewSimpleClientset(pod)
	p := newTestProvider(t)
	p.Clientset = cs
	p.trackPod(pod, &vm.VM{ID: "vmid", Name: "demo"})
	p.markLifecycleState(t.Context(), pod, meta.LifecycleStateReady, "")

	patches := 0
	cs.PrependReactor("patch", "pods", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		patches++
		return false, nil, nil
	})
	if err := p.UpdatePod(t.Context(), pod.DeepCopy()); err != nil {
		t.Fatalf("UpdatePod: %v", err)
	}
	if patches != 0 {
		t.Errorf("noop with unchanged gen must not patch, got %d patches", patches)
	}
}

func TestMarkLifecycleStateRejectsStaleObservedGeneration(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	cs := fake.NewSimpleClientset(pod)
	p := newTestProvider(t)
	p.Clientset = cs

	newPod := pod.DeepCopy()
	newPod.Annotations = map[string]string{meta.AnnotationCocoonSetGeneration: "5"}
	p.markLifecycleState(t.Context(), newPod, meta.LifecycleStateHibernating, "")

	stalePod := pod.DeepCopy()
	stalePod.Annotations = map[string]string{meta.AnnotationCocoonSetGeneration: "4"}
	p.markLifecycleState(t.Context(), stalePod, meta.LifecycleStateReady, "")

	key := meta.PodKey("ns", "demo-0")
	p.mu.RLock()
	got := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if got.status.State != meta.LifecycleStateHibernating || got.status.ObservedGeneration != 5 {
		t.Errorf("intent must keep newer hibernating/5, got %s/%d", got.status.State, got.status.ObservedGeneration)
	}
}

func TestRecordLifecycleFlushedSkipsForgottenPod(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	key := meta.PodKey("ns", "demo-0")
	p.recordLifecycleFlushed(key, "", "stale-snapshot")

	p.mu.RLock()
	_, present := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if present {
		t.Errorf("recordLifecycleFlushed must not resurrect entries for forgotten pods")
	}
}

func TestRecordLifecycleFlushedSkipsAdvancedIntent(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	key := meta.PodKey("ns", "demo-0")
	p.lifecycleIntent[key] = lifecycleEntry{status: meta.LifecycleStatus{State: meta.LifecycleStateHibernated, ObservedGeneration: 5}}

	older := meta.LifecycleStatus{State: meta.LifecycleStateCreating, ObservedGeneration: 4}
	p.recordLifecycleFlushed(key, "", older.Snapshot())

	p.mu.RLock()
	got := p.lifecycleIntent[key].flushed
	p.mu.RUnlock()
	if got != "" {
		t.Errorf("flushed snapshot must not be set when intent advanced, got %q", got)
	}
}

func TestFlushLifecycleSkipsWhenIntentAdvanced(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	cs := fake.NewSimpleClientset(pod)
	p := newTestProvider(t)
	p.Clientset = cs
	key := meta.PodKey("ns", "demo-0")
	advanced := meta.LifecycleStatus{State: meta.LifecycleStateReady, ObservedGeneration: 5}
	p.lifecycleIntent[key] = lifecycleEntry{status: advanced}

	patches := 0
	cs.PrependReactor("patch", "pods", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		patches++
		return false, nil, nil
	})
	stale := meta.LifecycleStatus{State: meta.LifecycleStateCreating, ObservedGeneration: 4}
	p.flushLifecycle(t.Context(), "ns", "demo-0", "", stale)
	if patches != 0 {
		t.Errorf("flushLifecycle must skip stale snapshot, got %d patches", patches)
	}
}

func TestFlushLifecycleDropsTrackingOnNotFound(t *testing.T) {
	t.Parallel()

	cs := fake.NewSimpleClientset()
	p := newTestProvider(t)
	p.Clientset = cs
	key := meta.PodKey("ns", "demo-0")
	status := meta.LifecycleStatus{State: meta.LifecycleStateReady, ObservedGeneration: 1}
	p.lifecycleIntent[key] = lifecycleEntry{status: status}
	p.recordLifecycleFlushed(key, "", status.Snapshot())

	p.flushLifecycle(t.Context(), "ns", "demo-0", "", status)

	p.mu.RLock()
	entry, intentStill := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if intentStill {
		t.Errorf("NotFound must drop tracking, entry=%+v", entry)
	}
}

func TestMarkLifecycleStateUpdatesTrackedPodAnnotations(t *testing.T) {
	t.Parallel()

	tracked := newPodWithSpec(meta.VMSpec{VMName: "demo", Mode: "run"})
	tracked.Annotations[meta.AnnotationCocoonSetGeneration] = "5"
	cs := fake.NewSimpleClientset(tracked)
	p := newTestProvider(t)
	p.Clientset = cs
	p.trackPod(tracked, nil)

	stale := tracked.DeepCopy()
	stale.Annotations[meta.AnnotationCocoonSetGeneration] = "3"
	p.markLifecycleState(t.Context(), stale, meta.LifecycleStateReady, "")

	if got := tracked.Annotations[meta.AnnotationLifecycleState]; got != "ready" {
		t.Errorf("tracked pod state = %q, want ready", got)
	}
	if got := tracked.Annotations[meta.AnnotationLifecycleObservedGeneration]; got != "5" {
		t.Errorf("tracked pod observed-generation = %q, want 5", got)
	}
}

func TestSeedLifecycleIntentFromPodRestoresIntent(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "demo-0", Namespace: "ns",
		Annotations: map[string]string{
			meta.AnnotationLifecycleState:              "ready",
			meta.AnnotationLifecycleObservedGeneration: "3",
		},
	}}
	p := newTestProvider(t)
	p.seedLifecycleIntentFromPod(pod)

	key := meta.PodKey("ns", "demo-0")
	p.mu.RLock()
	intent := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if intent.status.State != meta.LifecycleStateReady || intent.status.ObservedGeneration != 3 {
		t.Errorf("intent = %s/%d, want ready/3", intent.status.State, intent.status.ObservedGeneration)
	}
	if intent.flushed != intent.status.Snapshot() {
		t.Errorf("flushed must match intent on seed to avoid spurious reconcile")
	}
}

func TestSeedLifecycleIntentFromPodSkipsUnannotatedPod(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	p := newTestProvider(t)
	p.seedLifecycleIntentFromPod(pod)

	key := meta.PodKey("ns", "demo-0")
	p.mu.RLock()
	_, present := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if present {
		t.Errorf("must not seed intent for pod without lifecycle-state annotation")
	}
}

func TestRepublishAfterRestartWithSeed(t *testing.T) {
	t.Parallel()

	pod := newPodWithSpec(meta.VMSpec{VMName: "demo", Mode: "run"})
	pod.Annotations[meta.AnnotationCocoonSetGeneration] = "3"
	pod.Annotations[meta.AnnotationLifecycleState] = "ready"
	pod.Annotations[meta.AnnotationLifecycleObservedGeneration] = "3"
	cs := fake.NewSimpleClientset(pod)
	p := newTestProvider(t)
	p.Clientset = cs
	p.trackPod(pod, &vm.VM{ID: "vmid", Name: "demo"})
	p.seedLifecycleIntentFromPod(pod)

	bumped := pod.DeepCopy()
	bumped.Annotations[meta.AnnotationCocoonSetGeneration] = "5"
	if err := p.UpdatePod(t.Context(), bumped); err != nil {
		t.Fatalf("UpdatePod: %v", err)
	}

	updated, _ := cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if got := updated.Annotations[meta.AnnotationLifecycleObservedGeneration]; got != "5" {
		t.Errorf("observed-generation = %q, want 5 (republish after restart)", got)
	}
	if got := updated.Annotations[meta.AnnotationLifecycleState]; got != "ready" {
		t.Errorf("state = %q, want ready", got)
	}
}

func TestApplyLifecycleLockedDropsWriteFromRecreatedPodMismatchedUID(t *testing.T) {
	t.Parallel()

	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns", UID: "a"}}
	cs := fake.NewSimpleClientset(podA)
	p := newTestProvider(t)
	p.Clientset = cs
	p.trackPod(podA, nil)
	p.markLifecycleState(t.Context(), podA, meta.LifecycleStateReady, "")

	podB := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns", UID: "b"}}
	p.markLifecycleState(t.Context(), podB, meta.LifecycleStateFailed, "stale goroutine")

	key := meta.PodKey("ns", "demo-0")
	p.mu.RLock()
	got := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if got.status.State != meta.LifecycleStateReady {
		t.Errorf("intent state = %q, want ready (write from a recreated pod's stale goroutine must drop)", got.status.State)
	}
	if annoState := podA.Annotations[meta.AnnotationLifecycleState]; annoState != string(meta.LifecycleStateReady) {
		t.Errorf("pod A annotation = %q, want ready (must not be overwritten by the other incarnation)", annoState)
	}
}

func TestApplyLifecycleLockedSameGenFailedAllowsNewAttemptViaHibernating(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "demo-0", Namespace: "ns",
		Annotations: map[string]string{meta.AnnotationCocoonSetGeneration: "5"},
	}}
	cs := fake.NewSimpleClientset(pod)
	p := newTestProvider(t)
	p.Clientset = cs

	p.markLifecycleState(t.Context(), pod, meta.LifecycleStateFailed, "transient timeout")
	p.markLifecycleState(t.Context(), pod, meta.LifecycleStateHibernating, "")
	p.markLifecycleState(t.Context(), pod, meta.LifecycleStateHibernated, "")

	updated, _ := cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if got := updated.Annotations[meta.AnnotationLifecycleState]; got != string(meta.LifecycleStateHibernated) {
		t.Errorf("state = %q, want hibernated (a new attempt via Hibernating must un-stick same-gen Failed)", got)
	}
}

func TestApplyLifecycleLockedSameGenFailedStaysStickyWithoutNewAttempt(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "demo-0", Namespace: "ns",
		Annotations: map[string]string{meta.AnnotationCocoonSetGeneration: "5"},
	}}
	cs := fake.NewSimpleClientset(pod)
	p := newTestProvider(t)
	p.Clientset = cs

	p.markLifecycleState(t.Context(), pod, meta.LifecycleStateFailed, "transient timeout")
	p.markLifecycleState(t.Context(), pod, meta.LifecycleStateReady, "")

	updated, _ := cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if got := updated.Annotations[meta.AnnotationLifecycleState]; got != string(meta.LifecycleStateFailed) {
		t.Errorf("state = %q, want failed (Ready with no intervening new-attempt state must still drop)", got)
	}
}

func TestForgetPodDropsLifecycleState(t *testing.T) {
	t.Parallel()

	p := newTestProvider(t)
	key := meta.PodKey("ns", "demo-0")
	status := meta.LifecycleStatus{State: meta.LifecycleStateReady, ObservedGeneration: 1}
	p.lifecycleIntent[key] = lifecycleEntry{status: status}
	p.recordLifecycleFlushed(key, "", status.Snapshot())

	p.forgetPod("ns", "demo-0")

	p.mu.RLock()
	entry, intentStill := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if intentStill {
		t.Errorf("forgetPod must drop the lifecycle entry, got %+v", entry)
	}
}

func TestTrackPodPreservesReadyIntentOverStaleUpdate(t *testing.T) {
	t.Parallel()

	const ns, name = "ns", "demo-0"
	key := meta.PodKey(ns, name)
	p := newTestProvider(t)
	p.lifecycleIntent[key] = lifecycleEntry{status: meta.LifecycleStatus{State: meta.LifecycleStateReady, ObservedGeneration: 1}}

	stale := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: ns,
		Annotations: map[string]string{
			meta.AnnotationCocoonSetGeneration: "1",
			meta.AnnotationLifecycleState:      string(meta.LifecycleStateCreating),
		},
	}}
	p.trackPod(stale, nil)

	if got := p.pods[key].Annotations[meta.AnnotationLifecycleState]; got != string(meta.LifecycleStateReady) {
		t.Errorf("tracked lifecycle-state = %q, want ready (intent must win over stale UpdatePod)", got)
	}
}

func TestFlushLifecycleKeepsIntentStagedByNewerIncarnation(t *testing.T) {
	t.Parallel()

	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns", UID: "a"}}
	cs := fake.NewSimpleClientset(podA)
	p := newTestProvider(t)
	p.Clientset = cs

	key := meta.PodKey("ns", "demo-0")
	statusA := meta.LifecycleStatus{State: meta.LifecycleStateCreating, ObservedGeneration: 1}
	statusB := meta.LifecycleStatus{State: meta.LifecycleStateCreating, ObservedGeneration: 2}
	p.mu.Lock()
	p.lifecycleIntent[key] = lifecycleEntry{uid: "a", status: statusA}
	p.mu.Unlock()

	var patched []string
	cs.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patched = append(patched, string(action.(k8stesting.PatchAction).GetPatch()))
		if len(patched) > 1 {
			return false, nil, nil
		}
		p.mu.Lock()
		p.lifecycleIntent[key] = lifecycleEntry{uid: "b", status: statusB}
		p.mu.Unlock()
		return true, nil, apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), "demo-0", nil)
	})

	p.flushLifecycle(t.Context(), "ns", "demo-0", "a", statusA)

	p.mu.RLock()
	got, ok := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if !ok || got.uid != "b" || got.status != statusB {
		t.Fatalf("intent after superseded flush = %+v present=%v, want uid b with %+v", got, ok, statusB)
	}

	p.reconcileAllLifecycle(t.Context())

	if len(patched) != 2 || !strings.Contains(patched[1], `"uid":"b"`) {
		t.Fatalf("patches = %q, want a second one carrying uid b", patched)
	}
}

func TestFailCreateRepairsLifecycleWithOriginatingUID(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns", UID: "a"}}
	cs := fake.NewSimpleClientset(pod.DeepCopy())
	p := newTestProvider(t)
	p.Clientset = cs
	p.trackPod(pod, nil)

	var patched []string
	transient := true
	cs.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		body := string(action.(k8stesting.PatchAction).GetPatch())
		patched = append(patched, body)
		switch {
		case transient:
			return true, nil, apierrors.NewInternalError(errors.New("apiserver unavailable"))
		case !strings.Contains(body, `"uid":"a"`):
			return true, nil, apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), "demo-0", nil)
		}
		return false, nil, nil
	})

	if err := p.failCreate(t.Context(), pod, false, "CreateBringUpFailed", errors.New("boot failed")); err == nil {
		t.Fatal("failCreate must return the create error")
	}

	key := meta.PodKey("ns", "demo-0")
	p.mu.RLock()
	entry, ok := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if !ok || entry.uid != "a" {
		t.Fatalf("intent after failCreate = %+v present=%v, want it retained with uid a", entry, ok)
	}

	transient = false
	p.reconcileAllLifecycle(t.Context())

	p.mu.RLock()
	entry, ok = p.lifecycleIntent[key]
	p.mu.RUnlock()
	if !ok {
		t.Fatal("repair intent must survive reconciliation, or the failure never reaches the pod")
	}
	if last := patched[len(patched)-1]; !strings.Contains(last, `"uid":"a"`) {
		t.Errorf("repair patch = %s, want metadata.uid a", last)
	}
	if entry.flushed != entry.status.Snapshot() {
		t.Errorf("flushed = %q, want the repaired snapshot %q", entry.flushed, entry.status.Snapshot())
	}
}

func TestReconcileRepairsIncarnationReusingFlushedSnapshot(t *testing.T) {
	t.Parallel()

	podB := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns", UID: "b"}}
	cs := fake.NewSimpleClientset(podB.DeepCopy())
	p := newTestProvider(t)
	p.Clientset = cs

	key := meta.PodKey("ns", "demo-0")
	status := meta.LifecycleStatus{State: meta.LifecycleStateCreating}
	p.mu.Lock()
	p.lifecycleIntent[key] = lifecycleEntry{uid: "a", status: status, flushed: status.Snapshot()}
	p.mu.Unlock()

	var patched []string
	transient := true
	cs.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patched = append(patched, string(action.(k8stesting.PatchAction).GetPatch()))
		if transient {
			return true, nil, apierrors.NewInternalError(errors.New("apiserver unavailable"))
		}
		return false, nil, nil
	})

	p.trackPod(podB, nil)
	p.markLifecycleState(t.Context(), podB, meta.LifecycleStateCreating, "")

	transient = false
	p.reconcileAllLifecycle(t.Context())

	updated, _ := cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if got := updated.Annotations[meta.AnnotationLifecycleState]; got != string(meta.LifecycleStateCreating) {
		t.Errorf("apiserver state = %q, want creating (a predecessor's flushed marker must not mask B's drift)", got)
	}
	if len(patched) == 0 || !strings.Contains(patched[len(patched)-1], `"uid":"b"`) {
		t.Errorf("patches = %q, want a repair patch carrying uid b", patched)
	}

	p.mu.RLock()
	entry := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if entry.uid != "b" || entry.flushed != status.Snapshot() {
		t.Errorf("entry after repair = %+v, want uid b flushed %q", entry, status.Snapshot())
	}
}

func TestTrackPodDropsPredecessorLifecycleMarkers(t *testing.T) {
	t.Parallel()

	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "demo-0", Namespace: "ns", UID: "a",
		Annotations: map[string]string{meta.AnnotationCocoonSetGeneration: "5"},
	}}
	podB := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "demo-0", Namespace: "ns", UID: "b",
		Annotations: map[string]string{meta.AnnotationCocoonSetGeneration: "1"},
	}}
	cs := fake.NewSimpleClientset(podB.DeepCopy())
	p := newTestProvider(t)
	p.Clientset = cs
	p.trackPod(podA, nil)
	p.markLifecycleState(t.Context(), podA, meta.LifecycleStateFailed, "predecessor boom")

	p.trackPod(podB, nil)
	if got := podB.Annotations[meta.AnnotationLifecycleState]; got != "" {
		t.Errorf("tracked pod B carries the predecessor's state %q", got)
	}

	p.markLifecycleState(t.Context(), podB, meta.LifecycleStateCreating, "")
	if p.lifecycleAlreadyFailed(podB) {
		t.Error("B must not inherit the predecessor's Failed gate")
	}
	p.markReadyPublished(t.Context(), podB)

	key := meta.PodKey("ns", "demo-0")
	p.mu.RLock()
	entry := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if entry.uid != "b" || entry.status.State != meta.LifecycleStateReady || entry.status.ObservedGeneration != 1 {
		t.Errorf("entry = %+v, want uid b with ready at generation 1", entry)
	}

	updated, _ := cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if got := updated.Annotations[meta.AnnotationLifecycleState]; got != string(meta.LifecycleStateReady) {
		t.Errorf("apiserver state = %q, want ready", got)
	}
	if got := updated.Annotations[meta.AnnotationLifecycleObservedGeneration]; got != "1" {
		t.Errorf("apiserver observed-generation = %q, want 1", got)
	}
}

func TestUntrackedIntentRejectsPredecessorWrite(t *testing.T) {
	t.Parallel()

	podB := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns", UID: "b"}}
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns", UID: "a"}}
	cs := fake.NewSimpleClientset(podB.DeepCopy())
	p := newTestProvider(t)
	p.Clientset = cs
	p.trackPod(podB, nil)

	var patched []string
	transient := true
	cs.PrependReactor("patch", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		body := string(action.(k8stesting.PatchAction).GetPatch())
		patched = append(patched, body)
		switch {
		case transient:
			return true, nil, apierrors.NewInternalError(errors.New("apiserver unavailable"))
		case !strings.Contains(body, `"uid":"b"`):
			return true, nil, apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("Pod").GroupKind(), "demo-0", nil)
		}
		return false, nil, nil
	})

	if err := p.failCreate(t.Context(), podB, false, "CreateBringUpFailed", errors.New("boot failed")); err == nil {
		t.Fatal("failCreate must return the create error")
	}

	transient = false
	p.markLifecycleState(t.Context(), podA, meta.LifecycleStateFailed, "post-clone exec failed")

	key := meta.PodKey("ns", "demo-0")
	p.mu.RLock()
	entry, ok := p.lifecycleIntent[key]
	p.mu.RUnlock()
	if !ok || entry.uid != "b" || entry.status.Message != "boot failed" {
		t.Fatalf("entry after the predecessor's write = %+v present=%v, want B's own failed intent", entry, ok)
	}

	p.reconcileAllLifecycle(t.Context())

	if len(patched) == 0 || !strings.Contains(patched[len(patched)-1], `"uid":"b"`) {
		t.Errorf("patches = %q, want a repair patch carrying uid b", patched)
	}
	p.mu.RLock()
	entry = p.lifecycleIntent[key]
	p.mu.RUnlock()
	if entry.flushed != entry.status.Snapshot() {
		t.Errorf("flushed = %q, want the repaired snapshot %q", entry.flushed, entry.status.Snapshot())
	}
	updated, _ := cs.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if got := updated.Annotations[meta.AnnotationLifecycleStateMessage]; got != "boot failed" {
		t.Errorf("apiserver message = %q, want boot failed", got)
	}
}
