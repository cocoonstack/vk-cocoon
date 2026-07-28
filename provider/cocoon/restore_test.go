package cocoon

import (
	"errors"
	"strings"
	"sync"
	"testing"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"

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

func TestBringUpVMRestoreEnsuresOCIRefBaseImage(t *testing.T) {
	const vmName = "vk-ns-demo-0"
	rt := &fakeRuntime{
		snapshots: map[string]*vm.Snapshot{
			vmName: {
				Name:  vmName,
				Image: "simular/win11:25h2-20260705-1", // bare OCI ref: cocoon's --pull refuses these
			},
		},
	}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  vmName,
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	pod.Annotations[meta.AnnotationRestoreFromHibernate] = "true"
	spec := meta.ParseVMSpec(pod)

	if _, _, err := p.bringUpVM(t.Context(), pod, spec); err != nil {
		t.Fatalf("bringUpVM: %v", err)
	}
	if rt.cloned == nil {
		t.Fatal("restore must clone from the hibernate snapshot, Clone was never called")
	}
	if len(rt.ensuredImages) != 1 || rt.ensuredImages[0].image != "simular/win11:25h2-20260705-1" {
		t.Fatalf("OCI-ref base image was not ensured before restore, got %#v", rt.ensuredImages)
	}
	if !rt.cloned.Pull {
		t.Error("materialized OCI-ref base still needs --pull so core resolves the backing file by digest")
	}
}

func TestBringUpVMRestoreSkipsEnsureWhenDigestPresent(t *testing.T) {
	const vmName = "vk-ns-demo-0"
	rt := &fakeRuntime{
		snapshots: map[string]*vm.Snapshot{
			vmName: {
				Name:        vmName,
				Image:       "simular/win11:25h2-20260705-1",
				ImageDigest: "sha256:142ab794",
			},
		},
		imagesPresent: map[string]bool{"sha256:142ab794": true}, // same bytes under another name
	}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  vmName,
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	pod.Annotations[meta.AnnotationRestoreFromHibernate] = "true"
	spec := meta.ParseVMSpec(pod)

	if _, _, err := p.bringUpVM(t.Context(), pod, spec); err != nil {
		t.Fatalf("bringUpVM: %v", err)
	}
	if len(rt.ensuredImages) != 0 {
		t.Fatalf("EnsureImage should be skipped when the digest is already local, got %#v", rt.ensuredImages)
	}
	if len(rt.imageInspectCalls) != 1 || rt.imageInspectCalls[0] != "sha256:142ab794" {
		t.Fatalf("presence must be resolved by digest via Image(), got %#v", rt.imageInspectCalls)
	}
	if rt.cloned == nil || !rt.cloned.Pull {
		t.Error("restore clone must pass --pull when the base is present")
	}
}

func TestBringUpVMRestorePullsHTTPBase(t *testing.T) {
	const vmName = "vk-ns-demo-0"
	rt := &fakeRuntime{
		snapshots: map[string]*vm.Snapshot{
			vmName: {
				Name:  vmName,
				Image: "https://epoch.simular.cloud/dl/simular/win11/25h2-20260608",
			},
		},
	}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  vmName,
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	pod.Annotations[meta.AnnotationRestoreFromHibernate] = "true"
	spec := meta.ParseVMSpec(pod)

	if _, _, err := p.bringUpVM(t.Context(), pod, spec); err != nil {
		t.Fatalf("bringUpVM: %v", err)
	}
	if rt.cloned == nil {
		t.Fatal("Runtime.Clone was not called")
	}
	if !rt.cloned.Pull {
		t.Error("restore clone must pass --pull so core fetches a missing http(s) base")
	}
	if len(rt.ensuredImages) != 0 {
		t.Errorf("EnsureImage should not run for an http(s) base (core --pull handles it), got %#v", rt.ensuredImages)
	}
}

func TestCreatePodDerivesRestoreFromLocalEvidence(t *testing.T) {
	// Registry-less: a same-name local hibernate snapshot is the evidence.
	const vmName = "vk-ns-demo-0"
	rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{vmName: {Name: vmName}}}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{VMName: vmName, Image: "snapshot-repo:latest", Mode: "clone"})
	// Hammer the tracked pod's DeepCopy path: the derived marker is written
	// after trackPod, so it must take p.mu or -race flags this.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = p.GetPod(t.Context(), "ns", "demo-0")
			}
		}
	})
	err := p.CreatePod(t.Context(), pod)
	close(stop)
	readers.Wait()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	p.Close()
	if rt.cloned == nil || rt.cloned.From != vmName {
		t.Fatalf("evidence must route to the hibernate restore path, cloned = %#v", rt.cloned)
	}
	if !meta.ReadRestoreFromHibernate(pod) {
		t.Error("derived restore must mark the in-memory pod")
	}
}

func TestCreatePodEvidenceFailClosedOnRegistryError(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Registry = wakeVerifyRegistry{manifestErr: errors.New("registry down")}

	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Image: "snapshot-repo:latest", Mode: "clone"})
	err := p.CreatePod(t.Context(), pod)
	if err == nil || !strings.Contains(err.Error(), "registry down") {
		t.Fatalf("err = %v, want fail-closed evidence error", err)
	}
	if rt.cloned != nil || rt.ran != nil {
		t.Error("no VM may boot while hibernate evidence is unverifiable")
	}
}

func TestCreatePodEvidenceImageConflictRefusesBoot(t *testing.T) {
	const vmName = "vk-ns-demo-0"
	rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{vmName: {Name: vmName, ID: "SNAP-1"}}}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Registry = newWakeVerifyRegistryWithImage(t, "SNAP-1", "reg.example/agent:v1")

	pod := newPodWithSpec(meta.VMSpec{VMName: vmName, Image: "reg.example/agent:v2", Mode: "run"})
	err := p.CreatePod(t.Context(), pod)
	if err == nil || !strings.Contains(err.Error(), "came from image") {
		t.Fatalf("err = %v, want image-identity conflict", err)
	}
	if rt.cloned != nil || rt.ran != nil {
		t.Error("an image conflict must refuse both restore and fresh boot")
	}
}

func TestCreatePodEvidenceMatchingImageRestores(t *testing.T) {
	const vmName = "vk-ns-demo-0"
	rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{vmName: {Name: vmName, ID: "SNAP-1"}}}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Registry = newWakeVerifyRegistryWithImage(t, "SNAP-1", "reg.example/agent:v1")

	pod := newPodWithSpec(meta.VMSpec{VMName: vmName, Image: "reg.example/agent:v1", Mode: "run"})
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.cloned == nil || rt.cloned.From != vmName {
		t.Fatalf("matching identity must restore from the hibernate snapshot, cloned = %#v", rt.cloned)
	}
	if rt.ran != nil {
		t.Error("mode=run must not fresh-boot over hibernate evidence")
	}
}

func TestCreatePodEvidenceLegacyManifestRestores(t *testing.T) {
	// Hibernate artifacts pushed before the image annotation existed must
	// still veto the fresh boot.
	const vmName = "vk-ns-demo-0"
	rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{vmName: {Name: vmName, ID: "SNAP-1"}}}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Registry = newWakeVerifyRegistry(t, "SNAP-1")

	pod := newPodWithSpec(meta.VMSpec{VMName: vmName, Image: "snapshot-repo:latest", Mode: "clone"})
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.cloned == nil || rt.cloned.From != vmName {
		t.Fatalf("legacy evidence must still restore, cloned = %#v", rt.cloned)
	}
}

func TestCreatePodEvidenceForkFromConflict(t *testing.T) {
	const vmName = "vk-ns-demo-0"
	rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{vmName: {Name: vmName}}}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{VMName: vmName, ForkFrom: "vk-ns-main-0", Mode: "run"})
	err := p.CreatePod(t.Context(), pod)
	if err == nil || !strings.Contains(err.Error(), "explicit clone source") {
		t.Fatalf("err = %v, want explicit-source conflict", err)
	}
	if rt.cloned != nil || rt.ran != nil {
		t.Error("a source conflict must refuse both restore and fresh boot")
	}
}
