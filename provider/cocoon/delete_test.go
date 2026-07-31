package cocoon

import (
	"slices"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon-common/meta"

	"github.com/cocoonstack/vk-cocoon/vm"
)

// TestDeletePodSnapshotRetention locks the delete GC table: a plain delete removes the local snapshots, a seat release keeps them as the warm-wake cache.
func TestDeletePodSnapshotRetention(t *testing.T) {
	tests := []struct {
		name          string
		track         *vm.VM
		keep          bool
		wantRemovedID string
		wantSnapshots []string
	}{
		{
			name:          "forgotten vm removes snapshots",
			wantSnapshots: []string{"vk-ns-demo-0", forkSnapshotName("vk-ns-demo-0")},
		},
		{
			name: "seat release keeps snapshots",
			keep: true,
		},
		{
			name:          "seat release with live vm removes only the vm",
			track:         &vm.VM{ID: "live-vmid", Name: "vk-ns-demo-0", State: vm.StateRunning},
			keep:          true,
			wantRemovedID: "live-vmid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &fakeRuntime{}
			p := newTestProvider(t)
			p.Runtime = rt

			pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "clone"})
			if tt.keep {
				meta.MarkKeepSnapshotOnDelete(pod)
			}
			if tt.track != nil {
				p.trackPod(pod, tt.track)
			}

			if err := p.DeletePod(t.Context(), pod); err != nil {
				t.Fatalf("DeletePod: %v", err)
			}

			if rt.removedID != tt.wantRemovedID {
				t.Errorf("removedID = %q, want %q", rt.removedID, tt.wantRemovedID)
			}
			got := slices.Sorted(slices.Values(rt.snapshotRemoveCalls))
			want := slices.Sorted(slices.Values(tt.wantSnapshots))
			if !slices.Equal(got, want) {
				t.Errorf("snapshotRemoveCalls = %v, want %v", rt.snapshotRemoveCalls, tt.wantSnapshots)
			}
			if rt.snapshotSaveCount != 0 {
				t.Errorf("delete must not save a snapshot, got %d", rt.snapshotSaveCount)
			}
		})
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
