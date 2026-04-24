package cocoon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/network"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/vm"
)

type fakeRuntime struct {
	cloned        *vm.CloneOptions
	ran           *vm.RunOptions
	removedID     string
	savedSnapshot struct {
		name string
		vmID string
	}
	snapshotSaveCount   int
	snapshotRemoveCalls []string
	cloneErr            error
	runErr              error
	snapshotErr         error
	inspectErr          error
	cloneVM             *vm.VM
	inspectVM           *vm.VM
	runVM               *vm.VM
	snapshots           map[string]*vm.Snapshot
	listVMs             []vm.VM
	ensuredImages       []struct {
		image string
		force bool
	}

	// inspectSeq, when non-empty, is consumed in order by Inspect before
	// falling back to inspectErr/inspectVM. Lets tests script a sequence
	// of transient failures followed by a definitive result.
	inspectMu  sync.Mutex
	inspectSeq []fakeInspectStep
	inspectN   int
}

type fakeInspectStep struct {
	vm  *vm.VM
	err error
}

func (f *fakeRuntime) Clone(_ context.Context, opts vm.CloneOptions) (*vm.VM, error) {
	o := opts
	f.cloned = &o
	if f.cloneErr != nil {
		return nil, f.cloneErr
	}
	if f.cloneVM != nil {
		return f.cloneVM, nil
	}
	return &vm.VM{ID: "vmid-clone", Name: opts.To, IP: "10.0.0.10"}, nil
}

func (f *fakeRuntime) Run(_ context.Context, opts vm.RunOptions) (*vm.VM, error) {
	o := opts
	f.ran = &o
	if f.runErr != nil {
		return nil, f.runErr
	}
	if f.runVM != nil {
		return f.runVM, nil
	}
	return &vm.VM{ID: "vmid-run", Name: opts.Name, IP: "10.0.0.11"}, nil
}

func (f *fakeRuntime) Inspect(_ context.Context, _ string) (*vm.VM, error) {
	f.inspectMu.Lock()
	if f.inspectN < len(f.inspectSeq) {
		step := f.inspectSeq[f.inspectN]
		f.inspectN++
		f.inspectMu.Unlock()
		return step.vm, step.err
	}
	f.inspectMu.Unlock()
	if f.inspectErr != nil {
		return nil, f.inspectErr
	}
	if f.inspectVM != nil {
		return f.inspectVM, nil
	}
	return nil, fmt.Errorf("inspect: %w", vm.ErrVMNotFound)
}

func (f *fakeRuntime) List(_ context.Context) ([]vm.VM, error) { return f.listVMs, nil }

func (f *fakeRuntime) Remove(_ context.Context, vmID string) error {
	f.removedID = vmID
	return nil
}

func (f *fakeRuntime) SnapshotSave(_ context.Context, name, vmID string) error {
	f.savedSnapshot.name = name
	f.savedSnapshot.vmID = vmID
	f.snapshotSaveCount++
	if f.snapshots == nil {
		f.snapshots = map[string]*vm.Snapshot{}
	}
	f.snapshots[name] = &vm.Snapshot{Name: name}
	return nil
}

func (f *fakeRuntime) SnapshotRemoveIfExists(_ context.Context, name string) error {
	f.snapshotRemoveCalls = append(f.snapshotRemoveCalls, name)
	delete(f.snapshots, name)
	return nil
}

func (f *fakeRuntime) Snapshot(_ context.Context, name string) (*vm.Snapshot, error) {
	if f.snapshotErr != nil {
		return nil, f.snapshotErr
	}
	if f.snapshots != nil {
		if snapshot, ok := f.snapshots[name]; ok {
			return snapshot, nil
		}
	}
	return nil, errors.New("not found")
}

func (f *fakeRuntime) SnapshotImport(_ context.Context, _ vm.ImportOptions) (io.WriteCloser, func() error, error) {
	return nopWriteCloser{}, func() error { return nil }, nil
}

func (f *fakeRuntime) SnapshotExport(_ context.Context, _ string) (io.ReadCloser, func() error, error) {
	return io.NopCloser(nil), func() error { return nil }, nil
}

func (f *fakeRuntime) EnsureImage(_ context.Context, image string, force bool) error {
	f.ensuredImages = append(f.ensuredImages, struct {
		image string
		force bool
	}{image, force})
	return nil
}

func (f *fakeRuntime) Start(_ context.Context, _ string) error { return nil }

func (f *fakeRuntime) WatchEvents(_ context.Context) (<-chan vm.VMEvent, error) {
	ch := make(chan vm.VMEvent)
	close(ch)
	return ch, nil
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func newPodWithSpec(spec meta.VMSpec) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	spec.Managed = true
	spec.Apply(pod)
	return pod
}

func TestCreatePodMissingVMNameRejected(t *testing.T) {
	p := NewProvider()
	p.Runtime = &fakeRuntime{}
	p.Probes = probes.NewManager(t.Context())

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"}}
	if err := p.CreatePod(t.Context(), pod); err == nil {
		t.Errorf("expected error when VMName annotation is missing")
	}
}

func TestCreatePodCloneMode(t *testing.T) {
	rt := &fakeRuntime{
		snapshots: map[string]*vm.Snapshot{
			"snapshot-repo": {
				Name:  "snapshot-repo",
				Image: "https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64.img",
			},
		},
	}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "snapshot-repo:latest",
		Mode:   "clone",
	})
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.cloned == nil {
		t.Fatalf("Runtime.Clone was not called")
	}
	if rt.cloned.To != "vk-ns-demo-0" {
		t.Errorf("clone target: %q", rt.cloned.To)
	}
	if rt.cloned.From != "snapshot-repo" {
		t.Errorf("clone source: %q, want snapshot-repo", rt.cloned.From)
	}
	if !rt.cloned.Pull {
		t.Errorf("clone Pull = false, want true (base image should be auto-pulled)")
	}

	runtime := meta.ParseVMRuntime(pod)
	if runtime.VMID == "" {
		t.Errorf("VMID annotation was not written back")
	}
}

func TestCreatePodForkFromLocalVMSkipsSnapshotBaseImage(t *testing.T) {
	rt := &fakeRuntime{inspectVM: &vm.VM{ID: "source-vm-id", Name: "vk-ns-demo-0"}}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName:   "vk-ns-demo-1",
		Image:    "snapshot-repo:latest",
		Mode:     "clone",
		ForkFrom: "vk-ns-demo-0",
	})
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.cloned == nil {
		t.Fatalf("Runtime.Clone was not called")
	}
	if rt.savedSnapshot.name != "fork-vk-ns-demo-0" || rt.savedSnapshot.vmID != "source-vm-id" {
		t.Fatalf("SnapshotSave = (%q, %q), want (fork-vk-ns-demo-0, source-vm-id)", rt.savedSnapshot.name, rt.savedSnapshot.vmID)
	}
	if rt.cloned.From != "fork-vk-ns-demo-0" {
		t.Fatalf("clone source = %q, want fork-vk-ns-demo-0", rt.cloned.From)
	}
	if len(rt.ensuredImages) != 0 {
		t.Fatalf("EnsureImage should not run for local VM fork, got %#v", rt.ensuredImages)
	}
}

func TestCreatePodRunModeInvalidatesForkSnapshot(t *testing.T) {
	// A fresh main VM must drop the old fork snapshot so sub-agents cloned
	// after a main recreate pick up current state, not the stale checkpoint.
	rt := &fakeRuntime{runVM: &vm.VM{ID: "vmid-main", Name: "vk-ns-demo-0"}}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "epoch.example/cocoon/ubuntu:24.04",
		Mode:   "run",
		OS:     "linux",
	})
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(rt.snapshotRemoveCalls) != 1 || rt.snapshotRemoveCalls[0] != "fork-vk-ns-demo-0" {
		t.Fatalf("SnapshotRemoveIfExists calls = %v, want [fork-vk-ns-demo-0]", rt.snapshotRemoveCalls)
	}
}

func TestCreatePodForkFromReusesExistingSnapshot(t *testing.T) {
	// A cached fork snapshot from a previous sub-agent creation must
	// short-circuit the save; `snapshot save` pauses the source VM
	// and costs ~2s for a 1GiB guest, so hot-scale would be 4-5× slower
	// if we resnapshotted for every sub-agent. Fork invalidation is the
	// main VM's responsibility (see bringUpVM for mode=run).
	rt := &fakeRuntime{
		inspectVM: &vm.VM{ID: "source-vm-id", Name: "vk-ns-demo-0"},
		snapshots: map[string]*vm.Snapshot{
			"fork-vk-ns-demo-0": {Name: "fork-vk-ns-demo-0"},
		},
	}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName:   "vk-ns-demo-1",
		Image:    "snapshot-repo:latest",
		Mode:     "clone",
		ForkFrom: "vk-ns-demo-0",
	})
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.snapshotSaveCount != 0 {
		t.Fatalf("SnapshotSave should be skipped when snapshot exists, got count=%d", rt.snapshotSaveCount)
	}
	if rt.cloned == nil || rt.cloned.From != "fork-vk-ns-demo-0" {
		t.Fatalf("clone source = %q, want fork-vk-ns-demo-0", rt.cloned.From)
	}
}

func TestCreatePodForkFromOverridesRunMode(t *testing.T) {
	rt := &fakeRuntime{inspectVM: &vm.VM{ID: "source-vm-id", Name: "vk-ns-demo-0"}}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName:   "vk-ns-demo-2",
		Image:    "https://epoch.example.org/dl/windows/win11",
		Mode:     "run",
		OS:       "windows",
		ForkFrom: "vk-ns-demo-0",
	})
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.ran != nil {
		t.Fatalf("Runtime.Run should not be called when fork-from is set")
	}
	if rt.cloned == nil {
		t.Fatalf("Runtime.Clone was not called")
	}
	if rt.cloned.From != "fork-vk-ns-demo-0" {
		t.Fatalf("clone source = %q, want fork-vk-ns-demo-0", rt.cloned.From)
	}
}

func TestCreatePodRunMode(t *testing.T) {
	rt := &fakeRuntime{}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-toolbox",
		Image:  "ubuntu-22.04",
		Mode:   "run",
	})
	pod.Spec.Containers = []corev1.Container{{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1500m"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}}
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.ran == nil {
		t.Fatalf("Runtime.Run was not called")
	}
	if rt.cloned != nil {
		t.Errorf("Runtime.Clone should NOT have been called for mode=run")
	}
	if rt.ran.CPU != 2 {
		t.Fatalf("Run CPU = %d, want 2", rt.ran.CPU)
	}
	if rt.ran.Memory != "4294967296" {
		t.Fatalf("Run Memory = %q, want 4294967296", rt.ran.Memory)
	}
	if len(pod.Status.ContainerStatuses) != 1 || !pod.Status.ContainerStatuses[0].Ready {
		t.Fatalf("pod status was not refreshed to Ready: %#v", pod.Status.ContainerStatuses)
	}
}

func TestLocalSnapshotName(t *testing.T) {
	tests := []struct {
		repo, tag, want string
	}{
		{"myvm", "latest", "myvm"},
		{"myvm", "", "myvm"},
		{"myvm", "v2", "myvm:v2"},
		{"org/repo", "snapshot-v1", "org/repo:snapshot-v1"},
	}
	for _, tt := range tests {
		got := localSnapshotName(tt.repo, tt.tag)
		if got != tt.want {
			t.Errorf("localSnapshotName(%q, %q) = %q, want %q", tt.repo, tt.tag, got, tt.want)
		}
	}
}

func TestAssertSnapshotBackend(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		snapshot *vm.Snapshot
		target   string
		wantErr  bool
	}{
		{name: "nil snapshot accepts any target", snapshot: nil, target: "firecracker"},
		{name: "empty hypervisor accepts any target", snapshot: &vm.Snapshot{Name: "s"}, target: "firecracker"},
		{name: "empty target accepts any snapshot", snapshot: &vm.Snapshot{Name: "s", Hypervisor: "firecracker"}, target: ""},
		{name: "matching backends accepted", snapshot: &vm.Snapshot{Name: "s", Hypervisor: "firecracker"}, target: "firecracker"},
		{name: "ch snapshot vs fc target rejected", snapshot: &vm.Snapshot{Name: "s", Hypervisor: "cloud-hypervisor"}, target: "firecracker", wantErr: true},
		{name: "fc snapshot vs ch target rejected", snapshot: &vm.Snapshot{Name: "s", Hypervisor: "firecracker"}, target: "cloud-hypervisor", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := assertSnapshotBackend(tc.snapshot, tc.target)
			if (err != nil) != tc.wantErr {
				t.Fatalf("assertSnapshotBackend(%#v, %q) err = %v, wantErr = %v", tc.snapshot, tc.target, err, tc.wantErr)
			}
		})
	}
}

func TestCreatePodCloneModePropagatesBackend(t *testing.T) {
	rt := &fakeRuntime{
		snapshots: map[string]*vm.Snapshot{
			"snapshot-repo": {
				Name:       "snapshot-repo",
				Image:      "ghcr.io/x/y:1",
				Hypervisor: "firecracker",
			},
		},
	}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-fc",
		Image:   "snapshot-repo:latest",
		Mode:    "clone",
		Backend: "firecracker",
	})
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.cloned == nil {
		t.Fatalf("Runtime.Clone was not called")
	}
	if rt.cloned.Backend != "firecracker" {
		t.Errorf("Clone Backend = %q, want firecracker", rt.cloned.Backend)
	}
}

func TestCreatePodCloneModeRejectsBackendMismatch(t *testing.T) {
	rt := &fakeRuntime{
		snapshots: map[string]*vm.Snapshot{
			"snapshot-repo": {
				Name:       "snapshot-repo",
				Image:      "ghcr.io/x/y:1",
				Hypervisor: "cloud-hypervisor",
			},
		},
	}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-fc",
		Image:   "snapshot-repo:latest",
		Mode:    "clone",
		Backend: "firecracker",
	})
	err := p.CreatePod(t.Context(), pod)
	if err == nil {
		t.Fatalf("expected backend mismatch error, got nil")
	}
	if rt.cloned != nil {
		t.Errorf("Runtime.Clone should not have been called on backend mismatch")
	}
}

func TestCreatePodRunModePropagatesBackend(t *testing.T) {
	rt := &fakeRuntime{}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-fc-run",
		Image:   "ghcr.io/x/y:1",
		Mode:    "run",
		Backend: "firecracker",
	})
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.ran == nil {
		t.Fatalf("Runtime.Run was not called")
	}
	if rt.ran.Backend != "firecracker" {
		t.Errorf("Run Backend = %q, want firecracker", rt.ran.Backend)
	}
}

func TestCreatePodCloneModeWithTag(t *testing.T) {
	rt := &fakeRuntime{
		snapshots: map[string]*vm.Snapshot{
			"snapshot-repo:v2": {
				Name:  "snapshot-repo:v2",
				Image: "https://example.invalid/base.img",
			},
		},
	}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "snapshot-repo:v2",
		Mode:   "clone",
	})
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.cloned == nil {
		t.Fatalf("Runtime.Clone was not called")
	}
	if rt.cloned.From != "snapshot-repo:v2" {
		t.Errorf("clone source: %q, want snapshot-repo:v2", rt.cloned.From)
	}
}

func TestCreatePodCloneErrorPropagates(t *testing.T) {
	rt := &fakeRuntime{
		cloneErr: errors.New("cocoon vm clone boom"),
		snapshots: map[string]*vm.Snapshot{
			"snapshot-repo": {Name: "snapshot-repo", Image: "https://example.invalid/img.img"},
		},
	}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "snapshot-repo:latest",
		Mode:   "clone",
	})
	if err := p.CreatePod(t.Context(), pod); err == nil {
		t.Fatalf("expected clone error to surface, got nil")
	}
	if got := p.vmByName("vk-ns-demo-0"); got != nil {
		t.Errorf("failed CreatePod should not track a VM, got %#v", got)
	}
}

func TestCreatePodRunErrorPropagates(t *testing.T) {
	rt := &fakeRuntime{runErr: errors.New("cocoon vm run boom")}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-run",
		Image:  "ubuntu-22.04",
		Mode:   "run",
	})
	if err := p.CreatePod(t.Context(), pod); err == nil {
		t.Fatalf("expected run error to surface, got nil")
	}
	if got := p.vmByName("vk-ns-run"); got != nil {
		t.Errorf("failed CreatePod should not track a VM, got %#v", got)
	}
}

func TestDeletePodRemovesAndForgetsVM(t *testing.T) {
	rt := &fakeRuntime{}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName:         "vk-ns-demo-0",
		SnapshotPolicy: "never", // skip push path — not under test here
	})
	p.trackPod(pod, &vm.VM{ID: "vmid-del", Name: "vk-ns-demo-0"})
	p.Probes.Set(meta.PodKey("ns", "demo-0"), probes.Result{Ready: true, Live: true})

	if err := p.DeletePod(t.Context(), pod); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if rt.removedID != "vmid-del" {
		t.Errorf("Runtime.Remove got id=%q, want vmid-del", rt.removedID)
	}
	if got := p.vmByName("vk-ns-demo-0"); got != nil {
		t.Errorf("DeletePod should forget the VM, still tracked as %#v", got)
	}
	if _, err := p.GetPod(t.Context(), "ns", "demo-0"); err == nil {
		t.Errorf("DeletePod should drop the pod from the in-memory table")
	}
}

func TestCreatePodUnmanagedAdoptsExistingVM(t *testing.T) {
	rt := &fakeRuntime{}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	meta.VMSpec{VMName: "vk-ns-static", Mode: "static", Managed: false}.Apply(pod)
	meta.VMRuntime{VMID: "qemu-1", IP: "10.0.0.99"}.Apply(pod)

	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.cloned != nil || rt.ran != nil {
		t.Errorf("unmanaged mode should not call Clone or Run")
	}
}

func TestStartupReconcileAdoptsAnnotatedPods(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	pod.Spec.NodeName = "cocoon-pool"
	meta.VMRuntime{VMID: "adopted-vmid", IP: "10.0.0.42"}.Apply(pod)

	rt := &fakeRuntime{
		listVMs: []vm.VM{{ID: "adopted-vmid", Name: "vk-ns-demo-0", IP: "10.0.0.42"}},
	}
	p := NewProvider()
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())
	p.Clientset = fake.NewSimpleClientset(pod)

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if got := p.vmByName("vk-ns-demo-0"); got == nil || got.ID != "adopted-vmid" {
		t.Fatalf("adopted VM not tracked, got %#v", got)
	}
	if _, err := p.GetPod(t.Context(), "ns", "demo-0"); err != nil {
		t.Errorf("pod not adopted into in-memory table: %v", err)
	}
}

func TestStartupReconcileOrphanDestroyRemovesUnmatchedVM(t *testing.T) {
	rt := &fakeRuntime{
		listVMs: []vm.VM{{ID: "orphan-vmid", Name: "vk-ghost", IP: "10.0.0.99"}},
	}
	p := NewProvider()
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())
	p.Clientset = fake.NewSimpleClientset() // no pods
	p.OrphanPolicy = provider.OrphanDestroy

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if rt.removedID != "orphan-vmid" {
		t.Errorf("orphan VM should be removed under OrphanDestroy, removedID=%q", rt.removedID)
	}
}

func TestStartupReconcileAdoptsByVMNameWhenAnnotationMissing(t *testing.T) {
	// Simulate the post-crash state: CreatePod succeeded but the runtime
	// annotation patch failed, so the pod has no VMID yet the VM is live.
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	pod.Spec.NodeName = "cocoon-pool"
	// No VMRuntime applied.

	rt := &fakeRuntime{
		listVMs: []vm.VM{{ID: "live-vmid", Name: "vk-ns-demo-0", IP: "10.0.0.42"}},
	}
	p := NewProvider()
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())
	p.Clientset = fake.NewSimpleClientset(pod)
	// Under OrphanDestroy, a bug would delete the live VM. The fix must prevent that.
	p.OrphanPolicy = provider.OrphanDestroy

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if got := p.vmByName("vk-ns-demo-0"); got == nil || got.ID != "live-vmid" {
		t.Fatalf("VM should be re-adopted by name, got %#v", got)
	}
	if rt.removedID != "" {
		t.Fatalf("live VM must not be removed as orphan, removedID=%q", rt.removedID)
	}
	// Annotation patch is best-effort — verify it was at least attempted via the
	// patched pod's state when the fake clientset re-reads it.
	updated, err := p.Clientset.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if meta.ParseVMRuntime(updated).VMID != "live-vmid" {
		t.Errorf("annotation should be repaired, got %q", meta.ParseVMRuntime(updated).VMID)
	}
}

func TestStartupReconcileTracksHibernatedPodWithoutVM(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "clone", Managed: true})
	pod.Spec.NodeName = "cocoon-pool"
	// Simulate a hibernated pod: hibernate annotation set, no VMID.
	meta.HibernateState(true).Apply(pod)

	rt := &fakeRuntime{listVMs: nil}
	p := NewProvider()
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())
	p.Clientset = fake.NewSimpleClientset(pod)

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	// Pod must be tracked so v-k does not call CreatePod.
	if _, err := p.GetPod(t.Context(), "ns", "demo-0"); err != nil {
		t.Errorf("hibernated pod should be tracked: %v", err)
	}
	// No VM should be associated.
	if got := p.vmForPod("ns", "demo-0"); got != nil {
		t.Errorf("hibernated pod should have no VM, got %+v", got)
	}
}

func TestEvictPodKeepsStateOnAPIFailure(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	cs := fake.NewSimpleClientset(pod)
	cs.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api server unreachable")
	})

	p := NewProvider()
	p.Runtime = &fakeRuntime{}
	p.Probes = probes.NewManager(t.Context())
	p.Clientset = cs
	p.trackPod(pod, &vm.VM{ID: "vmid-evict", Name: "vk-ns-demo-0"})

	key := meta.PodKey(pod.Namespace, pod.Name)
	p.evictPod(t.Context(), key, pod, "VMGone", "vm no longer exists")

	if got := p.vmForPod("ns", "demo-0"); got == nil || got.ID != "vmid-evict" {
		t.Fatalf("VM should still be tracked after failed delete, got %#v", got)
	}
	if _, err := p.GetPod(t.Context(), "ns", "demo-0"); err != nil {
		t.Fatalf("pod should still be tracked after failed delete: %v", err)
	}
}

func TestEvictPodIdempotentOnNotFound(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	// Clientset has no pods — deletion returns NotFound, which evictPod must treat as success.
	cs := fake.NewSimpleClientset()

	p := NewProvider()
	p.Runtime = &fakeRuntime{}
	p.Probes = probes.NewManager(t.Context())
	p.Clientset = cs
	p.trackPod(pod, &vm.VM{ID: "vmid-evict", Name: "vk-ns-demo-0"})

	key := meta.PodKey(pod.Namespace, pod.Name)
	p.evictPod(t.Context(), key, pod, "VMGone", "vm no longer exists")

	if got := p.vmForPod("ns", "demo-0"); got != nil {
		t.Fatalf("VM should be untracked after NotFound delete, got %#v", got)
	}
	if _, err := p.GetPod(t.Context(), "ns", "demo-0"); err == nil {
		t.Fatalf("pod should be untracked after NotFound delete")
	}
}

func TestHandleVMGoneInlineRetryRecoversFromTransient(t *testing.T) {
	// First inspect fails transiently, second returns a running VM.
	// Pod must not be evicted; no deferred recheck needed.
	rt := &fakeRuntime{
		inspectSeq: []fakeInspectStep{
			{err: errors.New("exec: broken pipe")},
			{vm: &vm.VM{ID: "vmid-r", Name: "vk-ns-demo-0", State: vm.StateRunning}},
		},
	}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())
	p.Clientset = fake.NewSimpleClientset()
	p.inlineInspectBaseDelay = 1 * time.Millisecond

	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.trackPod(pod, &vm.VM{ID: "vmid-r", Name: "vk-ns-demo-0"})

	p.handleVMGone(t.Context(), &vm.VM{ID: "vmid-r", Name: "vk-ns-demo-0"})

	if got := p.vmForPod("ns", "demo-0"); got == nil {
		t.Fatalf("running VM should still be tracked after inline retry recovery")
	}
	if rt.inspectN != 2 {
		t.Fatalf("expected 2 inspect calls, got %d", rt.inspectN)
	}
}

func TestHandleVMGoneDeferredRecheckEvictsOnceDefinitive(t *testing.T) {
	// All inline retries transient; deferred recheck eventually sees NotFound.
	// notifyHook closes a channel once eviction fires — deterministic signal.
	rt := &fakeRuntime{
		inspectSeq: []fakeInspectStep{
			{err: errors.New("exec: broken pipe")},             // inline 1
			{err: errors.New("exec: broken pipe")},             // inline 2
			{err: errors.New("exec: broken pipe")},             // deferred 1 — still transient
			{err: fmt.Errorf("inspect: %w", vm.ErrVMNotFound)}, // deferred 2 — definitive
		},
	}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.Clientset = fake.NewSimpleClientset(pod)
	p.trackPod(pod, &vm.VM{ID: "vmid-d", Name: "vk-ns-demo-0"})
	p.inlineInspectBaseDelay = 1 * time.Millisecond
	p.deferredRecheckInitialDelay = 5 * time.Millisecond
	p.deferredRecheckMaxDelay = 20 * time.Millisecond

	evicted := make(chan struct{}, 1)
	p.NotifyPods(t.Context(), func(np *corev1.Pod) {
		if np.Status.Phase == corev1.PodFailed {
			select {
			case evicted <- struct{}{}:
			default:
			}
		}
	})

	p.handleVMGone(t.Context(), &vm.VM{ID: "vmid-d", Name: "vk-ns-demo-0"})

	select {
	case <-evicted:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred recheck did not evict pod within 2s")
	}
	if got := p.vmForPod("ns", "demo-0"); got != nil {
		t.Fatalf("deferred recheck should have evicted pod, still tracked as %#v", got)
	}
}

func TestHandleVMGoneDeferredRecheckHitsBudgetAndEvicts(t *testing.T) {
	// cocoon stays broken forever — budget expiration must evict so the pod
	// does not sit Running/NotReady indefinitely.
	rt := &fakeRuntime{inspectErr: errors.New("exec: broken pipe")}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.Clientset = fake.NewSimpleClientset(pod)
	p.trackPod(pod, &vm.VM{ID: "vmid-b", Name: "vk-ns-demo-0"})
	p.inlineInspectBaseDelay = 1 * time.Millisecond
	p.deferredRecheckInitialDelay = 5 * time.Millisecond
	p.deferredRecheckMaxDelay = 10 * time.Millisecond
	p.deferredRecheckBudget = 30 * time.Millisecond

	reasons := make(chan string, 4)
	p.NotifyPods(t.Context(), func(np *corev1.Pod) {
		if np.Status.Phase != corev1.PodFailed || len(np.Status.ContainerStatuses) == 0 {
			return
		}
		term := np.Status.ContainerStatuses[0].State.Terminated
		if term == nil {
			return
		}
		select {
		case reasons <- term.Reason:
		default:
		}
	})

	p.handleVMGone(t.Context(), &vm.VM{ID: "vmid-b", Name: "vk-ns-demo-0"})

	select {
	case got := <-reasons:
		if got != "VMInspectTimeout" {
			t.Fatalf("eviction reason = %q, want VMInspectTimeout", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("budget-timeout eviction did not fire within 2s")
	}
}

func TestHandleVMGoneDeferredRecheckDedups(t *testing.T) {
	// Two schedule calls for the same VM should yield a single pending entry.
	// We keep the first goroutine alive (slow delay) while the second attempts
	// to schedule, then drop the pod to let the goroutine exit before the test
	// returns so its view of the Provider fields does not race with cleanup.
	rt := &fakeRuntime{inspectErr: errors.New("exec: broken pipe")}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())
	p.Clientset = fake.NewSimpleClientset()
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.trackPod(pod, &vm.VM{ID: "vmid-x", Name: "vk-ns-demo-0"})
	p.deferredRecheckInitialDelay = 20 * time.Millisecond
	p.deferredRecheckMaxDelay = 50 * time.Millisecond

	p.scheduleDeferredRecheck("vmid-x")
	p.scheduleDeferredRecheck("vmid-x")

	p.mu.RLock()
	n := len(p.pendingRecheck)
	p.mu.RUnlock()
	if n != 1 {
		t.Fatalf("pendingRecheck should dedup, len=%d", n)
	}

	// Drop the pod so the goroutine exits on its next iteration, and wait for it.
	p.forgetPod("ns", "demo-0")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.RLock()
		done := len(p.pendingRecheck) == 0
		p.mu.RUnlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("deferred recheck goroutine did not exit after pod was forgotten")
}

func TestProviderCloseStopsDeferredRecheck(t *testing.T) {
	// A recheck goroutine running when Close is called must exit so the
	// provider drains cleanly instead of leaking.
	rt := &fakeRuntime{inspectErr: errors.New("exec: broken pipe")}
	p := NewProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())
	p.Clientset = fake.NewSimpleClientset()
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.trackPod(pod, &vm.VM{ID: "vmid-close", Name: "vk-ns-demo-0"})
	// Long enough that the goroutine is sleeping when we call Close.
	p.deferredRecheckInitialDelay = 5 * time.Second
	p.deferredRecheckMaxDelay = 10 * time.Second

	p.scheduleDeferredRecheck("vmid-close")

	done := make(chan struct{})
	go func() {
		p.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not drain deferred recheck goroutine within 2s")
	}

	// After Close, further schedule attempts must no-op.
	p.scheduleDeferredRecheck("vmid-close")
	p.mu.RLock()
	n := len(p.pendingRecheck)
	p.mu.RUnlock()
	if n != 0 {
		t.Fatalf("scheduleDeferredRecheck after Close should no-op, pendingRecheck=%d", n)
	}
}

func TestGetPodStatusRefreshesIPFromLease(t *testing.T) {
	p := NewProvider()
	p.Probes = probes.NewManager(t.Context())
	p.Probes.Set("ns/demo-0", probes.Result{Ready: true, Live: true})

	leasePath := filepath.Join(t.TempDir(), "leases.json")
	leases := `[{"mac":"aa:bb:cc:dd:ee:ff","ip":"172.20.0.88","expiry":"2099-01-01T00:00:00Z"}]`
	if err := os.WriteFile(leasePath, []byte(leases), 0o644); err != nil {
		t.Fatalf("write leases: %v", err)
	}
	p.LeaseParser = network.NewLeaseParser(leasePath)

	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.trackPod(pod, &vm.VM{ID: "vmid", Name: "vk-ns-demo-0", MAC: "aa:bb:cc:dd:ee:ff"})

	status, err := p.GetPodStatus(t.Context(), "ns", "demo-0")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if status.PodIP != "172.20.0.88" {
		t.Fatalf("PodIP = %q, want 172.20.0.88", status.PodIP)
	}
	if got := p.vmForPod("ns", "demo-0"); got == nil || got.IP != "172.20.0.88" {
		t.Fatalf("VM IP = %q, want 172.20.0.88", got.IP)
	}
}
