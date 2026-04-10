package main

import (
	"context"
	"errors"
	"io"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// fakeRuntime is a vm.Runtime stand-in for the provider tests. Each
// method records its arguments so the assertion side can verify
// what the provider asked for.
type fakeRuntime struct {
	cloned    *vm.CloneOptions
	ran       *vm.RunOptions
	removedID string
	cloneErr  error
	runErr    error
	cloneVM   *vm.VM
	runVM     *vm.VM
	listVMs   []vm.VM
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
	return nil, errors.New("not found")
}

func (f *fakeRuntime) List(_ context.Context) ([]vm.VM, error) { return f.listVMs, nil }

func (f *fakeRuntime) Remove(_ context.Context, vmID string) error {
	f.removedID = vmID
	return nil
}

func (f *fakeRuntime) SnapshotSave(_ context.Context, _, _ string) error { return nil }

func (f *fakeRuntime) SnapshotImport(_ context.Context, _ vm.ImportOptions) (io.WriteCloser, func() error, error) {
	return nopWriteCloser{}, func() error { return nil }, nil
}

func (f *fakeRuntime) SnapshotExport(_ context.Context, _ string) (io.ReadCloser, func() error, error) {
	return io.NopCloser(nil), func() error { return nil }, nil
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
	if err := p.CreatePod(context.Background(), pod); err == nil {
		t.Errorf("expected error when VMName annotation is missing")
	}
}

func TestCreatePodCloneMode(t *testing.T) {
	rt := &fakeRuntime{}
	p := NewCocoonProvider()
	p.Runtime = rt
	p.Probes = probes.NewManager()

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "ubuntu:24.04",
		Mode:   "clone",
	})
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.cloned == nil {
		t.Fatalf("Runtime.Clone was not called")
	}
	if rt.cloned.To != "vk-ns-demo-0" {
		t.Errorf("clone target: %q", rt.cloned.To)
	}

	// VMRuntime should be written back into the pod annotations.
	runtime := meta.ParseVMRuntime(pod)
	if runtime.VMID == "" {
		t.Errorf("VMID annotation was not written back")
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
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.ran == nil {
		t.Fatalf("Runtime.Run was not called")
	}
	if rt.cloned != nil {
		t.Errorf("Runtime.Clone should NOT have been called for mode=run")
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

	if err := p.CreatePod(context.Background(), pod); err != nil {
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
