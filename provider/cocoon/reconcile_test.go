package cocoon

import (
	"errors"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/vm"
)

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
	if len(rt.staleCreateCalls) != 1 || rt.staleCreateCalls[0] != "skel-vmid" {
		t.Errorf("reconcile-stale-create calls = %v, want [skel-vmid]", rt.staleCreateCalls)
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
	// An in-flight clone that survives the restart commits later; the watcher
	// must index it or the pod's create retries collide on the name forever.
	rt := &fakeRuntime{
		listVMs:             []vm.VM{{ID: "inflight-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
		staleCreateOutcomes: map[string]vm.StaleCreateOutcome{"inflight-vmid": vm.StaleCreateBusy},
		inspectSeq:          []fakeInspectStep{{vm: &vm.VM{ID: "inflight-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}}},
		inspectVM:           &vm.VM{ID: "inflight-vmid", Name: "vk-ns-demo-0", State: vm.StateRunning, IP: "10.0.0.7"},
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

func TestStartupReconcileSkeletonVerbErrorSkipsAdoption(t *testing.T) {
	// Also the mixed-version shape: an old cocoon binary without the verb
	// errors out, and vk must fail safe by leaving the record alone.
	rt := &fakeRuntime{
		listVMs:        []vm.VM{{ID: "skel-vmid", Name: "vk-ns-demo-0", State: vm.StateCreating}},
		staleCreateErr: errors.New(`unknown command "reconcile-stale-create"`),
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if got := p.vmByName("vk-ns-demo-0"); got != nil {
		t.Errorf("unreconciled placeholder must not be indexed, got %#v", got)
	}
	if rt.removedID != "" {
		t.Errorf("unreconciled placeholder must not be removed, removed %q", rt.removedID)
	}
}

func TestStartupReconcileSkeletonWithVMIDAnnotationNotAdopted(t *testing.T) {
	// The incident's second-restart shape: the previous incarnation already
	// wrote the skeleton's VMID onto the pod, so adoption would go through
	// the vmByID match instead of adoptByVMName.
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
