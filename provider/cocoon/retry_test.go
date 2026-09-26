package cocoon

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestAFailedHibernateIsRetriedWhenDueUntilItSucceeds(t *testing.T) {
	rt := &fakeRuntime{snapshotSaveErr: errors.New("save boom")}
	p, pod := newHibernateFixture(t, rt, "vmid-live", "10.0.0.7")
	meta.HibernateState(true).Apply(pod)
	p.trackPod(pod, &vm.VM{ID: "vmid-live", Name: "vk-ns-demo-0-505043", IP: "10.0.0.7", State: vm.StateRunning})
	saves := 0
	rt.snapshotSaveHook = func() { saves++ }
	key := meta.PodKey("ns", "demo-0")

	if err := p.UpdatePod(t.Context(), pod); err != nil {
		t.Fatalf("UpdatePod after a failed save: %v", err)
	}
	p.retryOp(t.Context(), key)
	if saves != 1 {
		t.Fatalf("saves before the retry is due = %d, want 1", saves)
	}
	forceRetryDue(t, p, key)
	p.retryOp(t.Context(), key)
	if r, _ := owedRetry(p, key); saves != 2 || r.delay != 30*time.Second {
		t.Fatalf("after one retry: saves = %d, next delay = %s, want 2 and 30s", saves, r.delay)
	}
	rt.snapshotSaveErr = nil
	forceRetryDue(t, p, key)
	p.retryOp(t.Context(), key)
	if _, owed := owedRetry(p, key); owed || p.vmForPod("ns", "demo-0") != nil {
		t.Fatalf("after a successful retry: retry owed = %v, VM tracked = %v, want neither", owed, p.vmForPod("ns", "demo-0") != nil)
	}
}

func TestOpRetryDelaysDoubleToTheirCap(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0-505043"})
	p.trackPod(pod, nil)
	key := meta.PodKey(pod.Namespace, pod.Name)
	var got []time.Duration
	var prev time.Duration
	for range 7 {
		start := time.Now()
		p.retryOpLater(t.Context(), pod, prev, errors.New("push refused"))
		r, _ := owedRetry(p, key)
		got = append(got, r.due.Sub(start).Round(time.Second))
		prev = r.delay
	}
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	if !slices.Equal(got, want) {
		t.Fatalf("retry delays = %v, want %v", got, want)
	}
}

func TestARetryYieldsToAnUpdateInFlight(t *testing.T) {
	rt := &fakeRuntime{}
	p, pod := newHibernateFixture(t, rt, "vmid-live", "10.0.0.7")
	meta.HibernateState(true).Apply(pod)
	p.trackPod(pod, &vm.VM{ID: "vmid-live", Name: "vk-ns-demo-0-505043", IP: "10.0.0.7", State: vm.StateRunning})
	key := meta.PodKey("ns", "demo-0")
	p.retryOpLater(t.Context(), pod, 0, errors.New("push refused"))
	forceRetryDue(t, p, key)

	l := p.podLock(key)
	l.Lock()
	p.retryOp(t.Context(), key)
	l.Unlock()

	if rt.snapshotSaveCount != 0 {
		t.Fatalf("saves = %d while an update held the pod, want 0", rt.snapshotSaveCount)
	}
	if _, owed := owedRetry(p, key); !owed {
		t.Fatal("the retry an update in flight deferred is no longer owed")
	}
}

func TestAFailedWakeOwesARetry(t *testing.T) {
	rt := &fakeRuntime{}
	p, pod := newHibernateFixture(t, rt, "", "")
	p.trackPod(pod, nil)
	key := meta.PodKey("ns", "demo-0")

	if err := p.UpdatePod(t.Context(), pod); err != nil {
		t.Fatalf("UpdatePod after a failed wake: %v", err)
	}
	if got, _ := p.GetPod(t.Context(), "ns", "demo-0"); meta.ReadLifecycleState(got) != meta.LifecycleStateFailed {
		t.Fatalf("lifecycle after the failed wake = %q, want failed", meta.ReadLifecycleState(got))
	}
	if _, owed := owedRetry(p, key); !owed {
		t.Fatal("a failed wake owes no retry")
	}
}

func TestAFailedResumedHibernateOwesARetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := &fakeRuntime{startErr: errors.New("start boom")}
		p, pod := newHibernateFixture(t, rt, "vmid-live", "10.0.0.7")
		meta.HibernateState(true).Apply(pod)
		v := &vm.VM{ID: "vmid-live", Name: "vk-ns-demo-0-505043", IP: "10.0.0.7", State: vm.StateRunning}
		p.trackPod(pod, v)
		key := meta.PodKey("ns", "demo-0")

		p.dispatchResume(key, pod, v, resumeOpHibernate)
		synctest.Wait()

		if _, owed := owedRetry(p, key); !owed {
			t.Fatal("a resumed hibernate whose start failed owes no retry")
		}
	})
}

func TestForgettingAPodDropsItsRetry(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0-505043"})
	p.trackPod(pod, nil)
	p.retryOpLater(t.Context(), pod, 0, errors.New("pull refused"))

	p.forgetPod("ns", "demo-0")

	if _, owed := owedRetry(p, meta.PodKey("ns", "demo-0")); owed {
		t.Fatal("a forgotten pod still owes a retry")
	}
}

func TestAFailureAfterThePodIsForgottenOwesNoRetry(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0-505043"})

	p.retryOpLater(t.Context(), pod, 0, errors.New("vm removed by the delete"))

	if _, owed := owedRetry(p, meta.PodKey("ns", "demo-0")); owed {
		t.Fatal("a pod no longer tracked owes a retry")
	}
}

func forceRetryDue(t *testing.T, p *Provider, key string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.opRetries[key]
	if !ok {
		t.Fatalf("no retry owed for %s", key)
	}
	r.due = time.Time{}
	p.opRetries[key] = r
}

func owedRetry(p *Provider, key string) (opRetry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.opRetries[key]
	return r, ok
}
