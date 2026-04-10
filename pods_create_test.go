package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/network"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// fakeRuntime is a vm.Runtime stand-in for the provider tests. Each
// method records its arguments so the assertion side can verify
// what the provider asked for.
type fakeRuntime struct {
	cloned        *vm.CloneOptions
	ran           *vm.RunOptions
	removedID     string
	savedSnapshot struct {
		name string
		vmID string
	}
	cloneErr      error
	runErr        error
	snapshotErr   error
	cloneVM       *vm.VM
	inspectVM     *vm.VM
	runVM         *vm.VM
	snapshots     map[string]*vm.Snapshot
	listVMs       []vm.VM
	ensuredImages []string
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
	if f.inspectVM != nil {
		return f.inspectVM, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeRuntime) List(_ context.Context) ([]vm.VM, error) { return f.listVMs, nil }

func (f *fakeRuntime) Remove(_ context.Context, vmID string) error {
	f.removedID = vmID
	return nil
}

func (f *fakeRuntime) SnapshotSave(_ context.Context, name, vmID string) error {
	f.savedSnapshot.name = name
	f.savedSnapshot.vmID = vmID
	if f.snapshots == nil {
		f.snapshots = map[string]*vm.Snapshot{}
	}
	f.snapshots[name] = &vm.Snapshot{Name: name}
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

func (f *fakeRuntime) EnsureImage(_ context.Context, image string) error {
	f.ensuredImages = append(f.ensuredImages, image)
	return nil
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func newPodWithSpec(spec meta.VMSpec) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	spec.Apply(pod)
	return pod
}

func TestCreatePodMissingVMNameRejected(t *testing.T) {
	p := NewCocoonProvider()
	p.Runtime = &fakeRuntime{}
	p.Probes = probes.NewManager()

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
	p := NewCocoonProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager()

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
	if len(rt.ensuredImages) != 1 || rt.ensuredImages[0] != rt.snapshots["snapshot-repo"].Image {
		t.Fatalf("EnsureImage calls = %#v, want [%q]", rt.ensuredImages, rt.snapshots["snapshot-repo"].Image)
	}

	// VMRuntime should be written back into the pod annotations.
	runtime := meta.ParseVMRuntime(pod)
	if runtime.VMID == "" {
		t.Errorf("VMID annotation was not written back")
	}
}

func TestCreatePodForkFromLocalVMSkipsSnapshotBaseImage(t *testing.T) {
	rt := &fakeRuntime{inspectVM: &vm.VM{ID: "source-vm-id", Name: "vk-ns-demo-0"}}
	p := NewCocoonProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager()

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

func TestCreatePodForkFromOverridesRunMode(t *testing.T) {
	rt := &fakeRuntime{inspectVM: &vm.VM{ID: "source-vm-id", Name: "vk-ns-demo-0"}}
	p := NewCocoonProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager()

	pod := newPodWithSpec(meta.VMSpec{
		VMName:   "vk-ns-demo-2",
		Image:    "https://epoch.simular.cloud/windows/win11",
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
	p := NewCocoonProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager()

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

func TestCreatePodStaticMode(t *testing.T) {
	rt := &fakeRuntime{}
	p := NewCocoonProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager()

	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-static", Mode: "static"})
	// Static mode requires the VMRuntime hints to already be set.
	meta.VMRuntime{VMID: "qemu-1", IP: "10.0.0.99"}.Apply(pod)

	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.cloned != nil || rt.ran != nil {
		t.Errorf("static mode should not call Clone or Run")
	}
}

func TestSplitRefHasTag(t *testing.T) {
	repo, tag := splitRef("foo:v1")
	if repo != "foo" || tag != "v1" {
		t.Errorf("got (%q, %q), want (foo, v1)", repo, tag)
	}
}

func TestSplitRefDefaultsLatest(t *testing.T) {
	repo, tag := splitRef("foo")
	if repo != "foo" || tag != "latest" {
		t.Errorf("got (%q, %q), want (foo, latest)", repo, tag)
	}
}

func TestShouldSnapshotOnDelete(t *testing.T) {
	cases := []struct {
		spec meta.VMSpec
		want bool
	}{
		{meta.VMSpec{SnapshotPolicy: "always", VMName: "vk-ns-demo-1"}, true},
		{meta.VMSpec{SnapshotPolicy: "never", VMName: "vk-ns-demo-0"}, false},
		{meta.VMSpec{SnapshotPolicy: "main-only", VMName: "vk-ns-demo-0"}, true},
		{meta.VMSpec{SnapshotPolicy: "main-only", VMName: "vk-ns-demo-1"}, false},
		{meta.VMSpec{SnapshotPolicy: "", VMName: "vk-ns-demo-0"}, true}, // default = always
	}
	for _, c := range cases {
		t.Run(string(c.spec.SnapshotPolicy)+"/"+c.spec.VMName, func(t *testing.T) {
			if got := shouldSnapshotOnDelete(c.spec); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestGetPodStatusRefreshesIPFromLease(t *testing.T) {
	p := NewCocoonProvider()
	p.Probes = probes.NewManager()
	p.Probes.MarkReady("ns/demo-0")

	leasePath := filepath.Join(t.TempDir(), "dnsmasq.leases")
	if err := os.WriteFile(leasePath, []byte("1775888313 aa:bb:cc:dd:ee:ff 172.20.0.88 demo *\n"), 0o644); err != nil {
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
	if meta.ParseVMRuntime(pod).IP != "172.20.0.88" {
		t.Fatalf("runtime annotation IP = %q, want 172.20.0.88", meta.ParseVMRuntime(pod).IP)
	}
}
