package cocoon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/vm"
)

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

func TestDeletePodRejectsWhileDeleteInFlight(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.trackPod(pod, &vm.VM{ID: "vmid-inflight", Name: "vk-ns-demo-0"})
	key := meta.PodKey("ns", "demo-0")
	p.mu.Lock()
	p.deleting[key] = struct{}{}
	p.mu.Unlock()

	err := p.DeletePod(t.Context(), pod)

	if err == nil || !strings.Contains(err.Error(), "delete operation still in flight") {
		t.Fatalf("DeletePod error = %v, want in-flight deletion", err)
	}
	if rt.removedID != "" {
		t.Fatalf("removed VM = %q, want none while a delete is in flight", rt.removedID)
	}
	if got := p.vmForPod("ns", "demo-0"); got == nil || got.ID != "vmid-inflight" {
		t.Fatalf("tracked VM = %#v, want vmid-inflight kept", got)
	}
	p.mu.Lock()
	held := p.deletingLocked(key)
	p.mu.Unlock()
	if !held {
		t.Fatal("rejected delete cleared the fence held by the in-flight delete")
	}
}

func TestDeletePodSkipsASupersededIncarnation(t *testing.T) {
	podA := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	podA.UID = "a"
	podB := podA.DeepCopy()
	podB.UID = "b"
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.trackPod(podB, &vm.VM{ID: "vmid-b", Name: "vk-ns-demo-0"})

	if err := p.DeletePod(t.Context(), podA); err != nil {
		t.Fatalf("DeletePod of the superseded incarnation: %v", err)
	}
	if rt.removedID != "" {
		t.Fatalf("removed VM = %q, want the successor's VM kept", rt.removedID)
	}
	if got := p.vmForPod("ns", "demo-0"); got == nil || got.ID != "vmid-b" {
		t.Fatalf("tracked VM = %#v, want the successor's vmid-b", got)
	}
	if uid, tracked := p.trackedPodUID(meta.PodKey("ns", "demo-0")); !tracked || uid != podB.UID {
		t.Fatalf("tracked UID = %q (%v), want %q", uid, tracked, podB.UID)
	}
}

func TestDeletePodReleasesAllDHCPLeases(t *testing.T) {
	rt := &fakeRuntime{}
	releaser := &recordingLeaseReleaser{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.LeaseReleaser = releaser
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "clone"})
	p.trackPod(pod, &vm.VM{
		ID:   "vmid-del",
		Name: "vk-ns-demo-0",
		NetworkConfigs: []*vm.NetworkConfig{
			{MAC: "aa:bb:cc:dd:ee:02"},
			{MAC: "aa:bb:cc:dd:ee:01"},
			{MAC: "aa:bb:cc:dd:ee:04", Network: &vm.NetworkInfo{}},
			{MAC: "aa:bb:cc:dd:ee:03", Network: &vm.NetworkInfo{IP: "10.0.0.3"}},
		},
	})

	if err := p.DeletePod(t.Context(), pod); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if rt.removedID != "vmid-del" {
		t.Fatalf("removed VM = %q, want vmid-del", rt.removedID)
	}
	got := slices.Sorted(slices.Values(releaser.released()))
	if want := []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02", "aa:bb:cc:dd:ee:04"}; !slices.Equal(got, want) {
		t.Errorf("released MACs = %v, want %v", got, want)
	}
}

func TestDeletePodLeaseReleaseFailureDoesNotResurrectVM(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.LeaseReleaser = &recordingLeaseReleaser{err: errors.New("cocoon-net unavailable")}
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "clone"})
	p.trackPod(pod, &vm.VM{ID: "vmid-del", Name: "vk-ns-demo-0", MAC: "aa:bb:cc:dd:ee:ff"})

	if err := p.DeletePod(t.Context(), pod); err != nil {
		t.Fatalf("lease cleanup after a successful VM remove must be best effort: %v", err)
	}
	if p.vmForPod(pod.Namespace, pod.Name) != nil {
		t.Fatal("VM remains tracked after successful removal")
	}
}

func TestDeletePodDoesNotReleaseLeaseWhenVMRemovalFails(t *testing.T) {
	releaser := &recordingLeaseReleaser{}
	p := newTestProvider(t)
	p.Runtime = &fakeRuntime{removeErr: errors.New("still running")}
	p.LeaseReleaser = releaser
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "clone"})
	p.trackPod(pod, &vm.VM{ID: "vmid-del", Name: "vk-ns-demo-0", MAC: "aa:bb:cc:dd:ee:ff"})

	if err := p.DeletePod(t.Context(), pod); err == nil {
		t.Fatal("expected VM removal error")
	}
	if got := releaser.released(); len(got) != 0 {
		t.Errorf("released MACs = %v before VM removal succeeded", releaser.macs)
	}
}

func TestDHCPMACsUsesLegacyPrimaryMACOnlyWithoutNICDetails(t *testing.T) {
	v := &vm.VM{MAC: " AA:BB:CC:DD:EE:FF "}
	if got := dhcpMACs(v); !slices.Equal(got, []string{"AA:BB:CC:DD:EE:FF"}) {
		t.Errorf("dhcpMACs = %v", got)
	}
}

func TestDHCPMACsSkipsStaticNICs(t *testing.T) {
	v := &vm.VM{
		MAC: "aa:bb:cc:dd:ee:ff",
		NetworkConfigs: []*vm.NetworkConfig{
			{MAC: "aa:bb:cc:dd:ee:ff", Network: &vm.NetworkInfo{IP: "10.0.0.2"}},
		},
	}
	if got := dhcpMACs(v); len(got) != 0 {
		t.Errorf("dhcpMACs = %v, want none for static NIC", got)
	}
}

type recordingLeaseReleaser struct {
	mu   sync.Mutex
	macs []string
	err  error
}

func (r *recordingLeaseReleaser) ReleaseByMAC(_ context.Context, mac string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.macs = append(r.macs, mac)
	return r.err
}

func (r *recordingLeaseReleaser) released() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.macs)
}

func (r *recordingLeaseReleaser) awaitReleases(t *testing.T, want int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := r.released(); len(got) >= want {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("releases = %v, want %d entries within 2s", r.released(), want)
	return nil
}
