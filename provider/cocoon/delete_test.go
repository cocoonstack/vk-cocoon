package cocoon

import (
	"testing"

	"github.com/cocoonstack/cocoon-common/meta"
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
