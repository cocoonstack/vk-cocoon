package cocoon

import (
	"errors"
	"testing"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/vm"
)

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
	// Pusher nil: hibernate skips epoch push but still does NetResize + Save + Remove.

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

func TestHibernateContinuesOnNICDropUnsupported(t *testing.T) {
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

	if err := p.hibernate(t.Context(), pod, v); err != nil {
		t.Fatalf("hibernate must degrade gracefully on ErrNetResizeUnsupported, got %v", err)
	}
	if rt.savedSnapshot.vmID != "vmid-2" {
		t.Errorf("snapshot save must still run after degraded NetResize, got %q", rt.savedSnapshot.vmID)
	}
}

func TestHibernateContinuesOnNICDropGenericErr(t *testing.T) {
	rt := &fakeRuntime{netResizeErr: errors.New("transient")}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	v := &vm.VM{ID: "vmid-3", Name: "vk-ns-demo-0"}

	if err := p.hibernate(t.Context(), pod, v); err != nil {
		t.Fatalf("hibernate must not fail on transient NetResize error, got %v", err)
	}
	if rt.savedSnapshot.vmID != "vmid-3" {
		t.Errorf("snapshot save must still run after warn-degraded NetResize, got %q", rt.savedSnapshot.vmID)
	}
}
