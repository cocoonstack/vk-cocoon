package cocoon

import (
	"testing"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestBringUpVMRestoreFromHibernate(t *testing.T) {
	cases := []struct {
		name     string
		os       string
		wantNICs bool // CH+Windows hibernate snapshots are NIC-less → clone hot-adds one
	}{
		{"windows+ch hot-adds a NIC", string(cocoonv1.OSWindows), true},
		{"linux inherits the snapshot NIC", string(cocoonv1.OSLinux), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const vmName = "vk-ns-demo-0"
			rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{vmName: {Name: vmName}}}
			p := newTestProvider(t)
			p.Runtime = rt
			p.Probes = probes.NewManager(t.Context())

			pod := newPodWithSpec(meta.VMSpec{
				VMName:  vmName,
				Backend: string(cocoonv1.BackendCloudHypervisor),
				OS:      tc.os,
			})
			pod.Annotations[meta.AnnotationRestoreFromHibernate] = "true"
			spec := meta.ParseVMSpec(pod)

			v, _, err := p.bringUpVM(t.Context(), pod, spec)
			if err != nil {
				t.Fatalf("bringUpVM: %v", err)
			}
			if v == nil {
				t.Fatal("bringUpVM returned nil vm")
			}
			if rt.cloned == nil {
				t.Fatal("restore must clone from the hibernate snapshot, Clone was never called")
			}
			if rt.cloned.From != vmName {
				t.Errorf("restore must clone from the local hibernate snapshot; From=%q want %q", rt.cloned.From, vmName)
			}
			gotNICs := rt.cloned.NICs != nil
			if gotNICs != tc.wantNICs {
				t.Errorf("NICs override present = %v, want %v", gotNICs, tc.wantNICs)
			}
			if tc.wantNICs && (rt.cloned.NICs == nil || *rt.cloned.NICs != 1) {
				t.Errorf("CH+Windows restore must clone with --nics 1; got %v", rt.cloned.NICs)
			}
		})
	}
}
