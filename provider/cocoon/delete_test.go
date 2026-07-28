package cocoon

import (
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// TestDeletePodForgottenVMRemovesLocalSnapshots locks the GC fix: deleting a pod whose VM was already forgotten must still remove its local snapshots.
func TestDeletePodForgottenVMRemovesLocalSnapshots(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	// Pod is never tracked → vmForPod returns nil (VM already forgotten).
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0"})

	if err := p.DeletePod(t.Context(), pod); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}

	removed := map[string]bool{}
	for _, name := range rt.snapshotRemoveCalls {
		removed[name] = true
	}
	if !removed["vk-ns-demo-0"] || !removed[forkSnapshotName("vk-ns-demo-0")] || len(removed) != 2 {
		t.Errorf("snapshotRemoveCalls = %v, want exactly [vk-ns-demo-0 %s]", rt.snapshotRemoveCalls, forkSnapshotName("vk-ns-demo-0"))
	}
	if rt.removedID != "" {
		t.Errorf("forgotten-VM delete must not call Runtime.Remove, got %q", rt.removedID)
	}
	if rt.snapshotSaveCount != 0 {
		t.Errorf("forgotten-VM delete must not save a snapshot, got %d", rt.snapshotSaveCount)
	}
}

func TestDeletePodBacksOffWhileResumeInFlight(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "clone"})

	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.trackPod(pod, &vm.VM{ID: "resume-vmid", Name: "vk-ns-demo-0", State: vm.StateRunning})

	key := meta.PodKey(pod.Namespace, pod.Name)
	if !p.claimResume(key) {
		t.Fatal("claim should succeed")
	}
	err := p.DeletePod(t.Context(), pod)
	if err == nil || !strings.Contains(err.Error(), "resumed operation") {
		t.Fatalf("err = %v, want resume backoff", err)
	}
	if rt.removedID != "" {
		t.Errorf("delete must not race the resume, removed %q", rt.removedID)
	}
	p.releaseResume(key)
	if err := p.DeletePod(t.Context(), pod); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if rt.removedID != "resume-vmid" {
		t.Errorf("delete should proceed after release, removed %q", rt.removedID)
	}
}
