package cocoon

import (
	"testing"

	"github.com/cocoonstack/cocoon-common/meta"
)

// TestDeletePodForgottenVMTouchesNoSnapshot locks the invariant the cross-node
// migration relies on: when the operator deletes the old pod, hibernate() has
// already removed + forgotten the VM, so DeletePod takes its v==nil early
// return and removes no snapshot. The epoch :hibernate checkpoint the target
// node restores from must survive the old pod's deletion.
func TestDeletePodForgottenVMTouchesNoSnapshot(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	// Pod is never tracked → vmForPod returns nil (VM already forgotten).
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0"})

	if err := p.DeletePod(t.Context(), pod); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if len(rt.snapshotRemoveCalls) != 0 {
		t.Errorf("forgotten-VM delete must remove no snapshot, got %v", rt.snapshotRemoveCalls)
	}
	if rt.removedID != "" {
		t.Errorf("forgotten-VM delete must not call Runtime.Remove, got %q", rt.removedID)
	}
	if rt.snapshotSaveCount != 0 {
		t.Errorf("forgotten-VM delete must not save a snapshot, got %d", rt.snapshotSaveCount)
	}
}
