package cocoon

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cocoonstack/cocoon-common/manifest"
	"github.com/cocoonstack/cocoon-common/meta"
	commonsnapshot "github.com/cocoonstack/cocoon-common/snapshot"
	"github.com/cocoonstack/vk-cocoon/snapshots"
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
			wantSnapshots: []string{"vk-ns-demo-0-505043", forkSnapshotName("vk-ns-demo-0-505043")},
		},
		{
			name: "seat release keeps snapshots",
			keep: true,
		},
		{
			name:          "seat release with live vm removes only the vm",
			track:         &vm.VM{ID: "live-vmid", Name: "vk-ns-demo-0-505043", State: vm.StateRunning},
			keep:          true,
			wantRemovedID: "live-vmid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &fakeRuntime{}
			p := newTestProvider(t)
			p.Runtime = rt

			pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0-505043", Mode: "clone"})
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

func TestDeletePodReadsAKeepFlagRefreshedFromTheInformer(t *testing.T) {
	const name = "vk-ns-demo-0-505043"
	tests := []struct {
		name          string
		updateUID     types.UID
		wantSnapshots []string
	}{
		{name: "flag patched after the last update keeps snapshots", updateUID: "uid-1"},
		{name: "a newer incarnation's flag is not this pod's", updateUID: "uid-2", wantSnapshots: []string{name, forkSnapshotName(name)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &fakeRuntime{}
			p := newTestProvider(t)
			p.Runtime = rt

			pod := newPodWithSpec(meta.VMSpec{VMName: name, Mode: "clone"})
			pod.UID = "uid-1"
			p.trackPod(pod, nil)

			update := pod.DeepCopy()
			update.UID = tt.updateUID
			meta.MarkKeepSnapshotOnDelete(update)
			p.RefreshKeepSnapshotOnDelete(update)

			tracked, err := p.GetPod(t.Context(), pod.Namespace, pod.Name)
			if err != nil {
				t.Fatalf("GetPod: %v", err)
			}
			if err := p.DeletePod(t.Context(), tracked); err != nil {
				t.Fatalf("DeletePod: %v", err)
			}
			got := slices.Sorted(slices.Values(rt.snapshotRemoveCalls))
			want := slices.Sorted(slices.Values(tt.wantSnapshots))
			if !slices.Equal(got, want) {
				t.Errorf("snapshotRemoveCalls = %v, want %v", got, want)
			}
		})
	}
}

func TestDeletePodLeavesAnUnmanagedVMAlone(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	pod := &corev1.Pod{Name: "cs-db", Namespace: "ns"}
	meta.VMSpec{VMName: "vk-ns-cs-db-bc21fc", Mode: "static", Managed: false}.Apply(pod)
	p.trackPod(pod, &vm.VM{ID: "extern-vm-1", Name: "vk-ns-cs-db-bc21fc", IP: "10.0.0.9", State: vm.StateRunning})

	if err := p.DeletePod(t.Context(), pod); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rt.removedID != "" || rt.snapshotSaveCount != 0 {
		t.Fatalf("delete touched an unmanaged VM: removed=%q saves=%d", rt.removedID, rt.snapshotSaveCount)
	}
	if p.vmForPod("ns", "cs-db") != nil {
		t.Fatal("the unmanaged pod was not forgotten")
	}
}

func TestDeletePodBacksOffWhileResumeInFlight(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0-505043", Mode: "clone"})

	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.trackPod(pod, &vm.VM{ID: "resume-vmid", Name: "vk-ns-demo-0-505043", State: vm.StateRunning})

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
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0-505043", Mode: "run"})
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.trackPod(pod, &vm.VM{ID: "vmid-inflight", Name: "vk-ns-demo-0-505043"})
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
	podA := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0-505043", Mode: "run"})
	podA.UID = "a"
	podB := podA.DeepCopy()
	podB.UID = "b"
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.trackPod(podB, &vm.VM{ID: "vmid-b", Name: "vk-ns-demo-0-505043"})

	if err := p.DeletePod(t.Context(), podA); err != nil {
		t.Fatalf("DeletePod of the superseded incarnation: %v", err)
	}
	if rt.removedID != "" {
		t.Fatalf("removed VM = %q, want the successor's VM kept", rt.removedID)
	}
	if got := p.vmForPod("ns", "demo-0"); got == nil || got.ID != "vmid-b" {
		t.Fatalf("tracked VM = %#v, want the successor's vmid-b", got)
	}
	if _, uid, tracked := p.trackedIncarnation(meta.PodKey("ns", "demo-0")); !tracked || uid != podB.UID {
		t.Fatalf("tracked UID = %q (%v), want %q", uid, tracked, podB.UID)
	}
}

func TestDeletePodSkipsASupersededIncarnationWhileAnotherDeleteIsInFlight(t *testing.T) {
	podA := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0-505043", Mode: "run"})
	podA.UID = "a"
	podB := podA.DeepCopy()
	podB.UID = "b"
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.trackPod(podB, &vm.VM{ID: "vmid-b", Name: "vk-ns-demo-0-505043"})
	key := meta.PodKey("ns", "demo-0")
	p.mu.Lock()
	p.deleting[key] = struct{}{}
	p.mu.Unlock()

	if err := p.DeletePod(t.Context(), podA); err != nil {
		t.Fatalf("DeletePod of the superseded incarnation: %v", err)
	}
	if rt.removedID != "" {
		t.Fatalf("removed VM = %q, want the successor's VM kept", rt.removedID)
	}
	p.mu.Lock()
	held := p.deletingLocked(key)
	p.mu.Unlock()
	if !held {
		t.Fatal("superseded delete released the fence held by the in-flight delete")
	}
}

func TestDeletePodReleasesAllDHCPLeases(t *testing.T) {
	rt := &fakeRuntime{}
	releaser := &recordingLeaseReleaser{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.LeaseReleaser = releaser
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0-505043", Mode: "clone"})
	p.trackPod(pod, &vm.VM{
		ID:   "vmid-del",
		Name: "vk-ns-demo-0-505043",
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
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0-505043", Mode: "clone"})
	p.trackPod(pod, &vm.VM{ID: "vmid-del", Name: "vk-ns-demo-0-505043", MAC: "aa:bb:cc:dd:ee:ff"})

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
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0-505043", Mode: "clone"})
	p.trackPod(pod, &vm.VM{ID: "vmid-del", Name: "vk-ns-demo-0-505043", MAC: "aa:bb:cc:dd:ee:ff"})

	if err := p.DeletePod(t.Context(), pod); !errors.Is(err, ErrDeleteKeptVM) {
		t.Fatalf("DeletePod = %v, want the removal failure marked for the delete retry", err)
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

func TestDeletePodKeepsTheVMWhenTheSnapshotFails(t *testing.T) {
	for _, stage := range []string{"inspect", "save", "push"} {
		t.Run(stage, func(t *testing.T) {
			running := &vm.VM{ID: "vmid-del", Name: "vk-ns-demo-0-505043", State: vm.StateRunning}
			rt := &snapshotExportRuntime{fakeRuntime: &fakeRuntime{inspectVM: running}, export: snapshotExportTar(t, running.Name)}
			switch stage {
			case "inspect":
				rt.inspectErr = errors.New("cocoon: transient")
			case "save":
				rt.snapshotSaveErr = errors.New("save interrupted")
			case "push":
				rt.exportErr = errors.New("registry unavailable")
			}
			p, pod := newSnapshotDeleteFixture(t, rt, running)

			err := p.DeletePod(t.Context(), pod)
			if !errors.Is(err, ErrDeleteKeptVM) || !strings.Contains(err.Error(), "before delete") {
				t.Fatalf("DeletePod = %v, want the snapshot failure marked for the delete retry", err)
			}
			if rt.removedID != "" {
				t.Fatalf("removed VM %q after a failed %s", rt.removedID, stage)
			}
			if got := p.vmForPod("ns", "demo-0"); got == nil || got.ID != running.ID {
				t.Fatalf("tracked VM = %#v, want %s kept for the delete retry", got, running.ID)
			}
			p.mu.RLock()
			held := p.deletingLocked(meta.PodKey("ns", "demo-0"))
			p.mu.RUnlock()
			if held {
				t.Fatal("a failed delete kept its deleting claim")
			}
		})
	}
}

func TestDeletePodSkipsTheSnapshotOfAVMThatIsNotRunning(t *testing.T) {
	stopped := &vm.VM{ID: "vmid-del", Name: "vk-ns-demo-0-505043", State: "stopped"}
	rt := &snapshotExportRuntime{fakeRuntime: &fakeRuntime{inspectVM: stopped}}
	p, pod := newSnapshotDeleteFixture(t, rt, stopped)

	if err := p.DeletePod(t.Context(), pod); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if rt.snapshotSaveCount != 0 {
		t.Errorf("snapshot saves = %d, want none for a stopped VM", rt.snapshotSaveCount)
	}
	if rt.removedID != stopped.ID || p.vmForPod("ns", "demo-0") != nil {
		t.Fatalf("removed %q tracked %#v, want the stopped VM removed and forgotten", rt.removedID, p.vmForPod("ns", "demo-0"))
	}
}

func TestDeletePodSnapshotsARunningVMBeforeRemovingIt(t *testing.T) {
	running := &vm.VM{ID: "vmid-del", Name: "vk-ns-demo-0-505043", State: vm.StateRunning}
	rt := &snapshotExportRuntime{fakeRuntime: &fakeRuntime{inspectVM: running}, export: snapshotExportTar(t, running.Name)}
	p, pod := newSnapshotDeleteFixture(t, rt, running)

	if err := p.DeletePod(t.Context(), pod); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if rt.snapshotSaveCount != 1 || rt.savedSnapshot.name != running.Name {
		t.Errorf("snapshot saves = %d of %q, want one of %q", rt.snapshotSaveCount, rt.savedSnapshot.name, running.Name)
	}
	if rt.removedID != running.ID || p.vmForPod("ns", "demo-0") != nil {
		t.Fatalf("removed %q tracked %#v, want the VM removed after the snapshot", rt.removedID, p.vmForPod("ns", "demo-0"))
	}
}

type snapshotExportRuntime struct {
	*fakeRuntime
	export    []byte
	exportErr error
}

func (r *snapshotExportRuntime) SnapshotExport(context.Context, string) (io.ReadCloser, func() error, error) {
	if r.exportErr != nil {
		return nil, nil, r.exportErr
	}
	return io.NopCloser(bytes.NewReader(r.export)), func() error { return nil }, nil
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
	synctest.Wait()
	got := r.released()
	if len(got) < want {
		t.Fatalf("releases = %v, want %d entries", got, want)
	}
	return got
}

func newSnapshotDeleteFixture(t *testing.T, rt *snapshotExportRuntime, v *vm.VM) (*Provider, *corev1.Pod) {
	t.Helper()
	p := newTestProvider(t)
	p.Runtime = rt
	p.Pusher = &snapshots.Pusher{Runtime: rt, Registry: fakeRegistry{}}
	pod := newPodWithSpec(meta.VMSpec{VMName: v.Name, Mode: "clone", SnapshotPolicy: "always"})
	p.trackPod(pod, v)
	return p, pod
}

func snapshotExportTar(t *testing.T, name string) []byte {
	t.Helper()
	envelope, err := commonsnapshot.MarshalEnvelope(&manifest.SnapshotConfig{SchemaVersion: "v1", SnapshotID: "SNAP-1"}, name)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "snapshot.json", Mode: 0o644, Size: int64(len(envelope))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(envelope); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
