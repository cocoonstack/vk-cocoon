package cocoon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestStartupReconcileRejectsIncompatibleBoundPod(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "clone"})
	pod.Spec.NodeName = "cocoon-pool"
	pod.Spec.NodeSelector = map[string]string{
		meta.LabelSnapshotCompatibilityClass: "n2-cascade-lake-v1",
	}

	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.SnapshotCompatibilityClass = "n4-emerald-rapids-v1"
	p.Runtime = &fakeRuntime{}
	p.Clientset = fake.NewSimpleClientset(pod)

	err := p.StartupReconcile(t.Context())
	if err == nil || !strings.Contains(err.Error(), "startup compatibility check") {
		t.Fatalf("StartupReconcile error = %v, want compatibility rejection", err)
	}
}

func TestStartupReconcileSkeletonCollectedNotAdopted(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "clone"})
	pod.Spec.NodeName = "cocoon-pool"

	rt := &fakeRuntime{
		listVMs: []vm.VM{{ID: "skel-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset(pod)

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if calls := rt.staleCalls(); len(calls) != 1 || calls[0] != "skel-vmid" {
		t.Errorf("reconcile-stale-create calls = %v, want [skel-vmid]", calls)
	}
	if got := p.vmByName("vk-ns-demo-0"); got != nil {
		t.Errorf("skeleton must not be indexed by name, got %#v", got)
	}
	if got := p.vmForPod("ns", "demo-0"); got != nil {
		t.Errorf("skeleton must not be adopted for the pod, got %#v", got)
	}
	if rt.removedID != "" {
		t.Errorf("vk must not rm the record itself (the verb owns reclaim), removed %q", rt.removedID)
	}
}

func TestStartupReconcileSkeletonBusyLeftAlone(t *testing.T) {
	rt := &fakeRuntime{
		listVMs:             []vm.VM{{ID: "inflight-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
		staleCreateOutcomes: map[string]vm.StaleCreateOutcome{"inflight-vmid": vm.StaleCreateBusy},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if got := p.vmByName("vk-ns-demo-0"); got != nil {
		t.Errorf("in-flight clone must not be indexed, got %#v", got)
	}
	if rt.removedID != "" {
		t.Errorf("in-flight clone must not be removed, removed %q", rt.removedID)
	}
}

func TestStartupReconcileBusyCreateIndexedAfterCommit(t *testing.T) {
	rt := &fakeRuntime{
		listVMs:             []vm.VM{{ID: "inflight-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
		staleCreateOutcomes: map[string]vm.StaleCreateOutcome{"inflight-vmid": vm.StaleCreateBusy},
		inspectSeq: []fakeInspectStep{
			{vm: &vm.VM{ID: "inflight-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
			{vm: &vm.VM{ID: "inflight-vmid", Name: "vk-ns-demo-0", State: vm.StateCreated}},
			{vm: &vm.VM{ID: "inflight-vmid", Name: "vk-ns-demo-0", State: vm.StateCreated}},
		},
		inspectVM: &vm.VM{ID: "inflight-vmid", Name: "vk-ns-demo-0", State: vm.StateRunning, IP: "10.0.0.7"},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()
	p.deferredRecheckInitialDelay = time.Millisecond
	p.deferredRecheckMaxDelay = 2 * time.Millisecond

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := p.vmByName("vk-ns-demo-0"); got != nil {
			if got.ID != "inflight-vmid" || got.State != vm.StateRunning {
				t.Fatalf("indexed VM = %#v, want the committed running record", got)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("committed in-flight create was never indexed for adoption")
}

func TestStartupReconcileBusyCreateDeadOnArrivalGetsOrphanPolicy(t *testing.T) {
	removed := make(chan struct{})
	rt := &fakeRuntime{
		listVMs:             []vm.VM{{ID: "inflight-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
		staleCreateOutcomes: map[string]vm.StaleCreateOutcome{"inflight-vmid": vm.StaleCreateBusy},
		inspectVM:           &vm.VM{ID: "inflight-vmid", Name: "vk-ns-demo-0", State: "error"},
		onRemove:            func() { close(removed) },
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()
	p.OrphanPolicy = provider.OrphanDestroy
	p.deferredRecheckInitialDelay = time.Millisecond
	p.deferredRecheckMaxDelay = 2 * time.Millisecond

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	select {
	case <-removed:
	case <-time.After(2 * time.Second):
		t.Fatal("dead-on-arrival create was never removed under OrphanDestroy")
	}
	p.Close()
	if rt.removedID != "inflight-vmid" {
		t.Errorf("removed %q, want inflight-vmid", rt.removedID)
	}
	if got := p.vmByName("vk-ns-demo-0"); got != nil {
		t.Errorf("dead record must not be indexed, got %#v", got)
	}
}

func TestWatchBusyCreateReclaimsAfterOwnerDies(t *testing.T) {
	rt := &fakeRuntime{
		listVMs: []vm.VM{{ID: "inflight-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
		staleCreateSeq: map[string][]vm.StaleCreateOutcome{
			"inflight-vmid": {vm.StaleCreateBusy, vm.StaleCreateBusy, vm.StaleCreateCollected},
		},
		inspectVM: &vm.VM{ID: "inflight-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()
	p.deferredRecheckInitialDelay = time.Millisecond
	p.deferredRecheckMaxDelay = 2 * time.Millisecond
	p.deferredRecheckBudget = 2 * time.Second

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(rt.staleCalls()) < 3 {
		time.Sleep(2 * time.Millisecond)
	}
	if calls := rt.staleCalls(); len(calls) < 3 {
		t.Fatalf("verb calls = %v, want the watcher to re-invoke until the record resolves", calls)
	}
	time.Sleep(50 * time.Millisecond)
	p.Close()
	if calls := rt.staleCalls(); len(calls) != 3 {
		t.Errorf("verb calls = %v, want the watch to stop once the record was collected", calls)
	}
	if got := p.vmByName("vk-ns-demo-0"); got != nil {
		t.Errorf("reclaimed record must not be indexed, got %#v", got)
	}
	if rt.removedID != "" {
		t.Errorf("vk must not rm the record itself (the verb owns reclaim), removed %q", rt.removedID)
	}
}

func TestStartupReconcileSkeletonNotCreatingReinspectsAndAdopts(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "clone"})
	pod.Spec.NodeName = "cocoon-pool"

	rt := &fakeRuntime{
		listVMs:             []vm.VM{{ID: "won-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
		staleCreateOutcomes: map[string]vm.StaleCreateOutcome{"won-vmid": vm.StaleCreateNotCreating},
		inspectVM:           &vm.VM{ID: "won-vmid", Name: "vk-ns-demo-0", State: vm.StateRunning, IP: "10.0.0.7"},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset(pod)

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	got := p.vmByName("vk-ns-demo-0")
	if got == nil || got.ID != "won-vmid" || got.State != vm.StateRunning {
		t.Fatalf("committed clone must be re-inspected and adopted, got %#v", got)
	}
}

func TestStartupReconcileNotCreatingCreatedKeepsWatching(t *testing.T) {
	rt := &fakeRuntime{
		listVMs:             []vm.VM{{ID: "won-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
		staleCreateOutcomes: map[string]vm.StaleCreateOutcome{"won-vmid": vm.StaleCreateNotCreating},
		inspectSeq: []fakeInspectStep{
			{vm: &vm.VM{ID: "won-vmid", Name: "vk-ns-demo-0", State: vm.StateCreated}},
			{vm: &vm.VM{ID: "won-vmid", Name: "vk-ns-demo-0", State: vm.StateCreated}},
		},
		inspectVM: &vm.VM{ID: "won-vmid", Name: "vk-ns-demo-0", State: vm.StateRunning, IP: "10.0.0.7"},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()
	p.deferredRecheckInitialDelay = time.Millisecond
	p.deferredRecheckMaxDelay = 2 * time.Millisecond

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := p.vmByName("vk-ns-demo-0"); got != nil {
			if got.State != vm.StateRunning {
				t.Fatalf("indexed VM state = %q, want running only", got.State)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("record was never indexed after reaching running")
}

func TestStartupReconcileNotCreatingInspectErrorKeepsWatching(t *testing.T) {
	rt := &fakeRuntime{
		listVMs:             []vm.VM{{ID: "won-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
		staleCreateOutcomes: map[string]vm.StaleCreateOutcome{"won-vmid": vm.StaleCreateNotCreating},
		inspectSeq:          []fakeInspectStep{{err: errors.New("cli hiccup")}},
		inspectVM:           &vm.VM{ID: "won-vmid", Name: "vk-ns-demo-0", State: vm.StateRunning, IP: "10.0.0.7"},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()
	p.deferredRecheckInitialDelay = time.Millisecond
	p.deferredRecheckMaxDelay = 2 * time.Millisecond

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := p.vmByName("vk-ns-demo-0"); got != nil {
			if got.State != vm.StateRunning {
				t.Fatalf("indexed VM state = %q, want running", got.State)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("committed VM was never indexed after the transient inspect failure")
}

func TestStartupReconcileNotCreatingDeadRecordGetsOrphanPolicy(t *testing.T) {
	rt := &fakeRuntime{
		listVMs:             []vm.VM{{ID: "won-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
		staleCreateOutcomes: map[string]vm.StaleCreateOutcome{"won-vmid": vm.StaleCreateNotCreating},
		inspectVM:           &vm.VM{ID: "won-vmid", Name: "vk-ns-demo-0", State: "stopped"},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()
	p.OrphanPolicy = provider.OrphanDestroy

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if rt.removedID != "won-vmid" {
		t.Errorf("stopped-without-running record must be removed under OrphanDestroy, removed %q", rt.removedID)
	}
	if got := p.vmByName("vk-ns-demo-0"); got != nil {
		t.Errorf("dead record must not be indexed, got %#v", got)
	}
}

func TestStartupReconcileVerbErrorWatchesWithoutAdopting(t *testing.T) {
	rt := &fakeRuntime{
		listVMs:        []vm.VM{{ID: "skel-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
		staleCreateErr: errors.New("cli hiccup"),
		inspectVM:      &vm.VM{ID: "skel-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()
	p.deferredRecheckInitialDelay = time.Millisecond
	p.deferredRecheckMaxDelay = 2 * time.Millisecond
	p.deferredRecheckBudget = 20 * time.Millisecond

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	p.Close()
	if got := p.vmByName("vk-ns-demo-0"); got != nil {
		t.Errorf("still-creating record must not be indexed, got %#v", got)
	}
	if rt.removedID != "" {
		t.Errorf("still-creating record must not be removed, removed %q", rt.removedID)
	}
}

func TestStartupReconcileVerbErrorCommittedRecordIndexed(t *testing.T) {
	rt := &fakeRuntime{
		listVMs:        []vm.VM{{ID: "won-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
		staleCreateErr: errors.New("cli hiccup"),
		inspectVM:      &vm.VM{ID: "won-vmid", Name: "vk-ns-demo-0", State: vm.StateRunning, IP: "10.0.0.7"},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()
	p.deferredRecheckInitialDelay = time.Millisecond
	p.deferredRecheckMaxDelay = 2 * time.Millisecond

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := p.vmByName("vk-ns-demo-0"); got != nil {
			if got.State != vm.StateRunning {
				t.Fatalf("indexed VM state = %q, want running", got.State)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("record committed after the verb error was never indexed")
}

func TestStartupReconcileSkeletonWithVMIDAnnotationNotAdopted(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "clone"})
	pod.Spec.NodeName = "cocoon-pool"
	meta.VMRuntime{VMID: "skel-vmid", IP: ""}.Apply(pod)

	rt := &fakeRuntime{
		listVMs: []vm.VM{{ID: "skel-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset(pod)

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if got := p.vmForPod("ns", "demo-0"); got != nil {
		t.Errorf("pod must not adopt the skeleton via its VMID annotation, got %#v", got)
	}
	if got := p.vmByName("vk-ns-demo-0"); got != nil {
		t.Errorf("skeleton must not be indexed by name, got %#v", got)
	}
}
