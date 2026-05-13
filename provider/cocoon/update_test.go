package cocoon

import (
	"errors"
	"testing"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestUseOnDemandClone(t *testing.T) {
	cases := []struct {
		name string
		spec meta.VMSpec
		want bool
	}{
		{"linux", meta.VMSpec{OS: string(cocoonv1.OSLinux)}, true},
		{"windows off", meta.VMSpec{OS: string(cocoonv1.OSWindows)}, false},
		{"android counts as non-windows", meta.VMSpec{OS: string(cocoonv1.OSAndroid)}, true},
		{"empty OS defaults to on", meta.VMSpec{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := useOnDemandClone(tc.spec); got != tc.want {
				t.Errorf("useOnDemandClone(%+v) = %v, want %v", tc.spec, got, tc.want)
			}
		})
	}
}

func TestShouldDropNICBeforeHibernate(t *testing.T) {
	cases := []struct {
		name string
		spec meta.VMSpec
		want bool
	}{
		{"ch+windows", meta.VMSpec{Backend: string(cocoonv1.BackendCloudHypervisor), OS: string(cocoonv1.OSWindows)}, true},
		{"ch+linux", meta.VMSpec{Backend: string(cocoonv1.BackendCloudHypervisor), OS: string(cocoonv1.OSLinux)}, false},
		{"fc+linux", meta.VMSpec{Backend: string(cocoonv1.BackendFirecracker), OS: string(cocoonv1.OSLinux)}, false},
		{"fc+windows", meta.VMSpec{Backend: string(cocoonv1.BackendFirecracker), OS: string(cocoonv1.OSWindows)}, false},
		{"empty backend defaults to false", meta.VMSpec{OS: string(cocoonv1.OSWindows)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldDropNICBeforeHibernate(tc.spec); got != tc.want {
				t.Errorf("shouldDropNICBeforeHibernate(%+v) = %v, want %v", tc.spec, got, tc.want)
			}
		})
	}
}

func TestHibernateDropsNICOnCHWindows(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	v := &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0"}

	if err := p.hibernate(t.Context(), pod, v); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	if len(rt.netResizeCalls) != 1 || rt.netResizeCalls[0].vmID != "vmid-1" || rt.netResizeCalls[0].target != 0 {
		t.Errorf("NetResize calls = %#v, want [{vmid-1 0}]", rt.netResizeCalls)
	}
	if rt.savedSnapshot.vmID != "vmid-1" {
		t.Errorf("SnapshotSave vmID = %q, want vmid-1", rt.savedSnapshot.vmID)
	}
	if rt.removedID != "vmid-1" {
		t.Errorf("Remove vmID = %q, want vmid-1", rt.removedID)
	}
}

func TestHibernateSkipsNICDropOnNonCHWindows(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		os      string
	}{
		{"ch+linux", string(cocoonv1.BackendCloudHypervisor), string(cocoonv1.OSLinux)},
		{"fc+linux", string(cocoonv1.BackendFirecracker), string(cocoonv1.OSLinux)},
		{"fc+windows", string(cocoonv1.BackendFirecracker), string(cocoonv1.OSWindows)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeRuntime{}
			p := newTestProvider(t)
			p.Runtime = rt
			p.Probes = probes.NewManager(t.Context())

			pod := newPodWithSpec(meta.VMSpec{
				VMName:  "vk-ns-demo-0",
				Backend: tc.backend,
				OS:      tc.os,
			})
			v := &vm.VM{ID: "vmid-x", Name: "vk-ns-demo-0"}

			if err := p.hibernate(t.Context(), pod, v); err != nil {
				t.Fatalf("hibernate: %v", err)
			}
			if len(rt.netResizeCalls) != 0 {
				t.Errorf("NetResize must not be called for %s, got %#v", tc.name, rt.netResizeCalls)
			}
			if rt.savedSnapshot.vmID == "" || rt.removedID == "" {
				t.Errorf("expected SnapshotSave + Remove to still run, got save=%q remove=%q", rt.savedSnapshot.vmID, rt.removedID)
			}
		})
	}
}

func TestHibernateFailsOnNICDropUnsupported(t *testing.T) {
	rt := &fakeRuntime{netResizeErr: vm.ErrNetResizeUnsupported}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	v := &vm.VM{ID: "vmid-2", Name: "vk-ns-demo-0"}

	err := p.hibernate(t.Context(), pod, v)
	if err == nil {
		t.Fatalf("hibernate must fail when NIC drop is unsupported on CH+Windows")
	}
	if !errors.Is(err, vm.ErrNetResizeUnsupported) {
		t.Errorf("error must wrap ErrNetResizeUnsupported, got %v", err)
	}
	if rt.savedSnapshot.vmID != "" {
		t.Errorf("snapshot save must not run after NIC drop failure, got %q", rt.savedSnapshot.vmID)
	}
	if meta.ReadLifecycleState(pod) != meta.LifecycleStateFailed {
		t.Errorf("lifecycle state = %q, want %q", meta.ReadLifecycleState(pod), meta.LifecycleStateFailed)
	}
}

func TestHibernateFailsOnNICDropGenericErr(t *testing.T) {
	dropErr := errors.New("transient")
	rt := &fakeRuntime{netResizeErr: dropErr}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	v := &vm.VM{ID: "vmid-3", Name: "vk-ns-demo-0"}

	err := p.hibernate(t.Context(), pod, v)
	if err == nil {
		t.Fatalf("hibernate must fail on transient NetResize error")
	}
	if !errors.Is(err, dropErr) {
		t.Errorf("error must wrap transient NetResize err, got %v", err)
	}
	if rt.savedSnapshot.vmID != "" {
		t.Errorf("snapshot save must not run after NIC drop failure, got %q", rt.savedSnapshot.vmID)
	}
	if meta.ReadLifecycleState(pod) != meta.LifecycleStateFailed {
		t.Errorf("lifecycle state = %q, want %q", meta.ReadLifecycleState(pod), meta.LifecycleStateFailed)
	}
}

func TestResolveWakeSourceUsesLocalSnapshot(t *testing.T) {
	rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{"vk-ns-demo-0": {Name: "vk-ns-demo-0"}}}
	p := newTestProvider(t)
	p.Runtime = rt

	got, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0")
	if err != nil {
		t.Fatalf("resolveWakeSource: %v", err)
	}
	if got != "vk-ns-demo-0" {
		t.Errorf("source = %q, want vk-ns-demo-0 (local snapshot)", got)
	}
}

func TestResolveWakeSourceErrorsWhenLocalMissingAndNoPuller(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	if _, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0"); err == nil {
		t.Fatal("expected error when local snapshot is missing and no Puller is set")
	}
}
