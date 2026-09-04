package cocoon

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestRestoreModeFor(t *testing.T) {
	cases := []struct {
		name string
		os   string
		mode vm.RestoreMode
		want vm.RestoreMode
	}{
		{"linux gets configured mode", string(cocoonv1.OSLinux), vm.RestoreMmap, vm.RestoreMmap},
		{"windows forced to copy", string(cocoonv1.OSWindows), vm.RestoreMmap, vm.RestoreCopy},
		{"android counts as non-windows", string(cocoonv1.OSAndroid), vm.RestoreOnDemand, vm.RestoreOnDemand},
		{"empty OS gets configured mode", "", vm.RestoreOnDemand, vm.RestoreOnDemand},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := restoreModeFor(tc.mode, tc.os); got != tc.want {
				t.Errorf("restoreModeFor(%q, %q) = %q, want %q", tc.mode, tc.os, got, tc.want)
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

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	v := &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0"}

	if err := p.hibernate(t.Context(), pod, meta.ParseVMSpec(pod), v); err != nil {
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

			pod := newPodWithSpec(meta.VMSpec{
				VMName:  "vk-ns-demo-0",
				Backend: tc.backend,
				OS:      tc.os,
			})
			v := &vm.VM{ID: "vmid-x", Name: "vk-ns-demo-0"}

			if err := p.hibernate(t.Context(), pod, meta.ParseVMSpec(pod), v); err != nil {
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

func TestHibernateClearsVMIDBeforeRemove(t *testing.T) {
	rt := &fakeRuntime{}
	p, pod := newHibernateFixture(t, rt, "vmid-pre", "10.0.0.7")

	var atRemove map[string]string
	rt.onRemove = func() {
		atRemove = maps.Clone(pod.Annotations)
	}

	v := &vm.VM{ID: "vmid-pre", Name: "vk-ns-demo-0"}
	if err := p.hibernate(t.Context(), pod, meta.ParseVMSpec(pod), v); err != nil {
		t.Fatalf("hibernate: %v", err)
	}

	if atRemove == nil {
		t.Fatalf("Remove was never invoked")
	}
	if got, ok := atRemove[meta.AnnotationVMID]; ok && got != "" {
		t.Errorf("VMID annotation must be cleared before Remove; got %q", got)
	}
	if got, ok := atRemove[meta.AnnotationIP]; ok && got != "" {
		t.Errorf("IP annotation must be cleared before Remove; got %q", got)
	}
	if got := pod.Annotations[meta.AnnotationVMID]; got != "" {
		t.Errorf("VMID annotation must remain cleared post-hibernate; got %q", got)
	}
}

func TestHibernateRestoresVMIDOnRemoveFailure(t *testing.T) {
	rmErr := errors.New("remove boom")
	rt := &fakeRuntime{removeErr: rmErr}
	p, pod := newHibernateFixture(t, rt, "vmid-live", "10.0.0.7")

	v := &vm.VM{ID: "vmid-live", Name: "vk-ns-demo-0", IP: "10.0.0.7"}
	err := p.hibernate(t.Context(), pod, meta.ParseVMSpec(pod), v)
	if !errors.Is(err, rmErr) {
		t.Fatalf("hibernate must wrap removeErr, got %v", err)
	}
	if got := pod.Annotations[meta.AnnotationVMID]; got != "vmid-live" {
		t.Errorf("VMID annotation must be restored after Remove failure; got %q want vmid-live", got)
	}
	if got := pod.Annotations[meta.AnnotationIP]; got != "10.0.0.7" {
		t.Errorf("IP annotation must be restored after Remove failure; got %q want 10.0.0.7", got)
	}
}

func TestHibernateKeepsVMIDOnSaveFailure(t *testing.T) {
	rt := &fakeRuntime{snapshotSaveErr: errors.New("save boom")}
	p, pod := newHibernateFixture(t, rt, "vmid-live", "10.0.0.7")

	v := &vm.VM{ID: "vmid-live", Name: "vk-ns-demo-0", IP: "10.0.0.7"}
	if err := p.hibernate(t.Context(), pod, meta.ParseVMSpec(pod), v); err == nil {
		t.Fatalf("hibernate must fail when SnapshotSave fails")
	}
	if got := pod.Annotations[meta.AnnotationVMID]; got != "vmid-live" {
		t.Errorf("VMID annotation must stay intact when hibernate aborts pre-Push; got %q", got)
	}
}

func TestHibernateFailsOnNICDropGenericErr(t *testing.T) {
	dropErr := errors.New("transient")
	rt := &fakeRuntime{netResizeErr: dropErr}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	v := &vm.VM{ID: "vmid-3", Name: "vk-ns-demo-0"}

	err := p.hibernate(t.Context(), pod, meta.ParseVMSpec(pod), v)
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

	src, err := p.resolveWakeSource(t.Context(), testWakePod(), "vk-ns-demo-0")
	if err != nil {
		t.Fatalf("resolveWakeSource: %v", err)
	}
	if src.localName != "vk-ns-demo-0" || src.dir != "" {
		t.Errorf("source = %+v, want local snapshot vk-ns-demo-0", src)
	}
	if src.snapshot == nil || src.snapshot.Name != "vk-ns-demo-0" {
		t.Errorf("snapshot metadata = %+v, want the local snapshot", src.snapshot)
	}
	src.release()
	if len(rt.snapshotRemoveCalls) != 0 {
		t.Errorf("local hit must keep the local snapshot; got removes %v", rt.snapshotRemoveCalls)
	}
}

func TestResolveWakeSourceErrorsWhenLocalMissingAndNoPuller(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	if _, err := p.resolveWakeSource(t.Context(), testWakePod(), "vk-ns-demo-0"); err == nil {
		t.Fatal("expected error when local snapshot is missing and no Puller is set")
	}
}

func testWakePod() *corev1.Pod {
	return newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "clone"})
}

func TestFinalizeDropNICWakeMarksReadyWhenIPArrives(t *testing.T) {
	p, pod, v := newDropNICWakeFixture(t, 2*time.Second, 1*time.Millisecond)
	p.Probes.Set(meta.PodKey("ns", "demo-0"), probes.Result{Ready: true})

	done := make(chan struct{})
	go func() {
		p.markReadyAfterIP(t.Context(), pod, meta.ParseVMSpec(pod), v, true)
		close(done)
	}()

	time.AfterFunc(10*time.Millisecond, func() {
		p.setVMIP("ns", "demo-0", v.ID, "172.20.1.228")
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("finalizeDropNICWake did not return within budget")
	}
	if got := meta.ReadLifecycleState(pod); got != meta.LifecycleStateReady {
		t.Fatalf("lifecycle = %q, want %q", got, meta.LifecycleStateReady)
	}
	if got := pod.Status.PodIP; got != "172.20.1.228" {
		t.Fatalf("pod.Status.PodIP = %q, want 172.20.1.228 (must be published before Ready)", got)
	}
}

func TestFinalizeDropNICWakePublishesIPBeforeReady(t *testing.T) {
	p, pod, v, client := newWakeClientsetFixture(t)

	var mu sync.Mutex
	var seq []string
	client.PrependReactor("*", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if a, ok := action.(k8stesting.PatchActionImpl); ok {
			switch {
			case a.GetSubresource() == "status" && strings.Contains(string(a.GetPatch()), `"podIP":"172.20.1.228"`):
				seq = append(seq, "status ip=172.20.1.228")
			case a.GetSubresource() == "" && strings.Contains(string(a.GetPatch()), string(meta.LifecycleStateReady)):
				seq = append(seq, "ready")
			}
		}
		return false, nil, nil
	})

	done := make(chan struct{})
	go func() {
		p.markReadyAfterIP(t.Context(), pod, meta.ParseVMSpec(pod), v, true)
		close(done)
	}()
	time.AfterFunc(10*time.Millisecond, func() {
		p.setVMIP("ns", "demo-0", v.ID, "172.20.1.228")
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("markReadyAfterIP did not return within budget")
	}

	mu.Lock()
	defer mu.Unlock()
	publishedAt := slices.Index(seq, "status ip=172.20.1.228")
	readyAt := slices.Index(seq, "ready")
	if publishedAt < 0 {
		t.Fatalf("podIP was never published to the apiserver, writes = %v", seq)
	}
	if readyAt < 0 {
		t.Fatalf("ready was never patched, writes = %v", seq)
	}
	if publishedAt > readyAt {
		t.Errorf("ready patched before the podIP reached the apiserver, writes = %v", seq)
	}
}

func TestFinalizeDropNICWakeMarksFailedOnTimeout(t *testing.T) {
	p, pod, v := newDropNICWakeFixture(t, 20*time.Millisecond, 1*time.Millisecond)

	p.markReadyAfterIP(t.Context(), pod, meta.ParseVMSpec(pod), v, true)

	if got := meta.ReadLifecycleState(pod); got != meta.LifecycleStateFailed {
		t.Fatalf("lifecycle = %q, want %q", got, meta.LifecycleStateFailed)
	}
}

func TestFinalizeDropNICWakeSkipsLifecycleWhenVMForgotten(t *testing.T) {
	p, pod, v := newDropNICWakeFixture(t, 2*time.Second, 1*time.Millisecond)

	done := make(chan struct{})
	go func() {
		p.markReadyAfterIP(t.Context(), pod, meta.ParseVMSpec(pod), v, true)
		close(done)
	}()

	time.AfterFunc(10*time.Millisecond, func() {
		p.forgetVMOnly("ns", "demo-0")
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("finalizeDropNICWake did not return after VM was forgotten")
	}
	if got := meta.ReadLifecycleState(pod); got != meta.LifecycleStateCreating {
		t.Fatalf("lifecycle = %q, want %q (untouched after VM forget)", got, meta.LifecycleStateCreating)
	}
}

func TestFinalizeDropNICWakeSkipsLifecycleWhenHibernateRequested(t *testing.T) {
	p, pod, v := newDropNICWakeFixture(t, 2*time.Second, 1*time.Millisecond)

	done := make(chan struct{})
	go func() {
		p.markReadyAfterIP(t.Context(), pod, meta.ParseVMSpec(pod), v, true)
		close(done)
	}()

	time.AfterFunc(10*time.Millisecond, func() {
		hib := pod.DeepCopy()
		meta.HibernateState(true).Apply(hib)
		p.trackPod(hib, nil)
		p.setVMIP("ns", "demo-0", v.ID, "172.20.1.228")
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("finalizeDropNICWake did not return after hibernate request")
	}
	if got := meta.ReadLifecycleState(pod); got != meta.LifecycleStateCreating {
		t.Fatalf("lifecycle = %q, want %q (must not race hibernate)", got, meta.LifecycleStateCreating)
	}
}

func TestHibernateReleasesLeaseBeforeNICDrop(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	v := &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0"}

	if err := p.hibernate(t.Context(), pod, meta.ParseVMSpec(pod), v); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	if got := execArgvs(rt); len(got) != 1 || got[0] != "cmd /c ipconfig /release" {
		t.Errorf("exec calls = %v, want exactly [cmd /c ipconfig /release]", got)
	}
	if len(rt.netResizeCalls) != 1 || rt.netResizeCalls[0].target != 0 {
		t.Errorf("NetResize calls = %#v, want one drop-to-0", rt.netResizeCalls)
	}
}

func TestHibernateReleaseFailureDoesNotBlock(t *testing.T) {
	rt := &fakeRuntime{execErr: errors.New("agent down")}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	if err := p.hibernate(t.Context(), pod, meta.ParseVMSpec(pod), &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0"}); err != nil {
		t.Fatalf("hibernate must proceed past a failed release: %v", err)
	}
	if rt.removedID != "vmid-1" {
		t.Errorf("hibernate did not complete: removedID=%q", rt.removedID)
	}
}

func TestHibernateSkipsReleaseOnNonDropNIC(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSLinux),
	})
	if err := p.hibernate(t.Context(), pod, meta.ParseVMSpec(pod), &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0"}); err != nil {
		t.Fatalf("hibernate: %v", err)
	}
	if len(rt.execCalls) != 0 {
		t.Errorf("linux hibernate must not exec ipconfig, got %v", execArgvs(rt))
	}
}

func TestHibernateRollbackRenews(t *testing.T) {
	rt := &fakeRuntime{snapshotSaveErr: errors.New("save boom")}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	if err := p.hibernate(t.Context(), pod, meta.ParseVMSpec(pod), &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0"}); err == nil {
		t.Fatal("hibernate should fail on snapshot save error")
	}
	got := execArgvs(rt)
	if len(got) != 2 || got[0] != "cmd /c ipconfig /release" || got[1] != "cmd /c ipconfig /renew" {
		t.Errorf("exec calls = %v, want [release, renew-on-rollback]", got)
	}
	if len(rt.netResizeCalls) != 2 || rt.netResizeCalls[1].target != 1 {
		t.Errorf("NetResize calls = %#v, want drop then rollback re-add", rt.netResizeCalls)
	}
}

func TestHibernateRenewsWhenNICDropFails(t *testing.T) {
	rt := &fakeRuntime{netResizeErr: errors.New("resize boom")}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	if err := p.hibernate(t.Context(), pod, meta.ParseVMSpec(pod), &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0"}); err == nil {
		t.Fatal("hibernate should fail when the NIC drop fails")
	}
	got := execArgvs(rt)
	if len(got) != 2 || got[0] != "cmd /c ipconfig /release" || got[1] != "cmd /c ipconfig /renew" {
		t.Errorf("exec calls = %v, want [release, renew] around the failed drop", got)
	}
	if len(rt.netResizeCalls) != 1 {
		t.Errorf("NetResize calls = %#v, want only the failed drop (NIC never dropped, no re-add)", rt.netResizeCalls)
	}
}

func TestHibernateRenewsEvenWhenReleaseVerdictUnknown(t *testing.T) {
	rt := &fakeRuntime{execErr: errors.New("vsock timeout"), netResizeErr: errors.New("resize boom")}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	if err := p.hibernate(t.Context(), pod, meta.ParseVMSpec(pod), &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0"}); err == nil {
		t.Fatal("hibernate should fail when the NIC drop fails")
	}
	got := execArgvs(rt)
	if len(got) != 2 || got[1] != "cmd /c ipconfig /renew" {
		t.Errorf("exec calls = %v, want a renew attempt even though the release verdict was an error", got)
	}
}

func TestHibernateRenewSurvivesCancelledContext(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()

	ctx, cancel := context.WithCancel(t.Context())
	rt.onExec = func() { cancel() }

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	if err := p.hibernate(ctx, pod, meta.ParseVMSpec(pod), &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0"}); err == nil {
		t.Fatal("hibernate should fail when the NIC drop fails")
	}
	got := execArgvs(rt)
	if len(got) != 2 || got[1] != "cmd /c ipconfig /renew" {
		t.Errorf("exec calls = %v, want the renew to outlive the cancelled request", got)
	}
}

func TestHibernateRollbackSurvivesCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	rt := &fakeRuntime{snapshotSaveErr: errors.New("save boom"), snapshotSaveHook: cancel}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	if err := p.hibernate(ctx, pod, meta.ParseVMSpec(pod), &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0"}); err == nil {
		t.Fatal("hibernate should fail on snapshot save error")
	}
	if len(rt.netResizeCalls) != 2 || rt.netResizeCalls[1].target != 1 {
		t.Errorf("NetResize calls = %#v, want the NIC re-add to outlive the cancelled request", rt.netResizeCalls)
	}
	if got := execArgvs(rt); len(got) != 2 || got[1] != "cmd /c ipconfig /renew" {
		t.Errorf("exec calls = %v, want the rollback renew to share the detached lifetime", got)
	}
}

func TestResolveVMIPRefusesWriteToSwappedVM(t *testing.T) {
	p := newTestProvider(t)
	p.LeaseParser = newLeaseParser(t, "aa:bb:cc:dd:ee:ff", "172.20.0.10")
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0"})
	p.trackPod(pod, &vm.VM{ID: "vmid-successor", Name: "vk-ns-demo-0"})

	stale := &vm.VM{ID: "vmid-old", Name: "vk-ns-demo-0", MAC: "aa:bb:cc:dd:ee:ff"}
	if ip := p.resolveVMIP("ns", "demo-0", stale); ip != "" {
		t.Errorf("stale VM's lease resolved to %q, want refused", ip)
	}
	if got := p.vmForPod("ns", "demo-0").IP; got != "" {
		t.Errorf("successor VM polluted with IP %q", got)
	}
}

func TestWaitForFreshIPBailsWhenVMSwapped(t *testing.T) {
	p, pod, _ := newDropNICWakeFixture(t, 500*time.Millisecond, 10*time.Millisecond)
	rt := p.Runtime.(*fakeRuntime)
	p.trackPod(pod, &vm.VM{ID: "vmid-successor", Name: "vk-ns-demo-0", IP: "10.0.0.9"})

	if p.waitForFreshIP(t.Context(), pod, meta.ParseVMSpec(pod), "vmid-wake") {
		t.Fatal("waiter armed for a replaced VM must fail, not adopt the successor")
	}
	if len(rt.execCalls) != 0 {
		t.Errorf("waiter must not exec into the successor VM, got %v", execArgvs(rt))
	}
}

func TestWaitForFreshIPRenewNudgeWhenLeaseMissing(t *testing.T) {
	p, pod, _ := newDropNICWakeFixture(t, 200*time.Millisecond, 10*time.Millisecond)
	rt := p.Runtime.(*fakeRuntime)
	p.wakeRenewNudgeDelay = 30 * time.Millisecond

	if p.waitForFreshIP(t.Context(), pod, meta.ParseVMSpec(pod), "vmid-wake") {
		t.Fatal("no IP should time out")
	}
	got := execArgvs(rt)
	if len(got) != 1 || got[0] != "cmd /c ipconfig /renew" {
		t.Errorf("exec calls = %v, want exactly one renew nudge", got)
	}
}

func TestWaitForFreshIPNoRenewWhenIPPresent(t *testing.T) {
	p, pod, v := newDropNICWakeFixture(t, 200*time.Millisecond, 10*time.Millisecond)
	rt := p.Runtime.(*fakeRuntime)
	p.wakeRenewNudgeDelay = time.Nanosecond
	v.IP = "10.0.0.9"

	if !p.waitForFreshIP(t.Context(), pod, meta.ParseVMSpec(pod), "vmid-wake") {
		t.Fatal("IP present should return true")
	}
	if len(rt.execCalls) != 0 {
		t.Errorf("guest already holds a lease; renew must not fire, got %v", execArgvs(rt))
	}
}

func TestWaitForFreshIPNoRenewForLinux(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.wakeFreshIPBudget = 120 * time.Millisecond
	p.wakeFreshIPInterval = 10 * time.Millisecond
	p.wakeRenewNudgeDelay = 20 * time.Millisecond

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSLinux),
	})
	p.trackPod(pod, &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0"})

	if p.waitForFreshIP(t.Context(), pod, meta.ParseVMSpec(pod), "vmid-1") {
		t.Fatal("no IP should time out")
	}
	if len(rt.execCalls) != 0 {
		t.Errorf("linux guest must not get ipconfig, got %v", execArgvs(rt))
	}
}

func TestWaitForFreshIPLeaseLandingDuringNudgeWins(t *testing.T) {
	p, pod, _ := newDropNICWakeFixture(t, 100*time.Millisecond, 10*time.Millisecond)
	rt := p.Runtime.(*fakeRuntime)
	p.wakeRenewNudgeDelay = 20 * time.Millisecond

	rt.onExec = func() {
		time.Sleep(150 * time.Millisecond)
		p.setVMIP("ns", "demo-0", "vmid-wake", "10.0.0.9")
	}

	if !p.waitForFreshIP(t.Context(), pod, meta.ParseVMSpec(pod), "vmid-wake") {
		t.Fatal("lease landed during the nudge exec; verdict must be success")
	}
}

func TestWakeClearsStalePostCloneMarker(t *testing.T) {
	const vmName = "vk-ns-demo-0"
	rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{vmName: {Name: vmName}}}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{VMName: vmName, Mode: "clone"})
	pod.Annotations[annotationPostCloneState] = postCloneStateDone

	if err := p.UpdatePod(t.Context(), pod); err != nil {
		t.Fatalf("wake: %v", err)
	}
	p.Close()
	if rt.cloned == nil || rt.cloned.From != vmName {
		t.Fatalf("wake must clone from the hibernate snapshot, cloned = %#v", rt.cloned)
	}
	if v, ok := pod.Annotations[annotationPostCloneState]; ok && v == postCloneStateDone {
		t.Errorf("stale done marker must not describe the new incarnation, got %q", v)
	}
}

func TestWakeSupersededSkipsNotify(t *testing.T) {
	const vmName = "vk-ns-demo-0"
	podA := newPodWithSpec(meta.VMSpec{VMName: vmName, Mode: "clone"})
	podA.UID = "a"
	podB := podA.DeepCopy()
	podB.UID = "b"

	p := newTestProvider(t)
	p.Runtime = &fakeRuntime{snapshots: map[string]*vm.Snapshot{vmName: {Name: vmName}}}
	client := fake.NewSimpleClientset(podB)
	client.PrependReactor("patch", "pods", rejectMismatchedUID(string(podB.UID)))
	p.Clientset = client
	notified := make(chan *corev1.Pod, 1)
	p.notifyHook = func(updated *corev1.Pod) { notified <- updated }

	if err := p.UpdatePod(t.Context(), podA); err != nil {
		t.Fatalf("update: %v", err)
	}
	p.Close()

	select {
	case got := <-notified:
		t.Errorf("superseded wake published status for uid %q, want no notify", got.UID)
	default:
	}
}

func TestUpdatePodReadsPodBeforeTracking(t *testing.T) {
	p := newTestProvider(t)
	base := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Backend: string(cocoonv1.BackendCloudHypervisor)})
	key := meta.PodKey(base.Namespace, base.Name)
	p.trackPod(base.DeepCopy(), &vm.VM{ID: "vmid-1", Name: "vk-ns-demo-0"})

	hammerPodAnnotation(t, p, key)
	for range 200 {
		if err := p.UpdatePod(t.Context(), base.DeepCopy()); err != nil {
			t.Fatalf("UpdatePod: %v", err)
		}
	}
}

func TestGetPodsReturnsCopies(t *testing.T) {
	p := newTestProvider(t)
	base := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Backend: string(cocoonv1.BackendCloudHypervisor)})
	key := meta.PodKey(base.Namespace, base.Name)
	p.trackPod(base.DeepCopy(), nil)

	hammerPodAnnotation(t, p, key)
	for range 200 {
		pods, err := p.GetPods(t.Context())
		if err != nil || len(pods) != 1 {
			t.Fatalf("GetPods = %v, %v, want one pod", pods, err)
		}
		_ = pods[0].Annotations[annotationPostCloneState]
	}
}

func hammerPodAnnotation(t *testing.T, p *Provider, key string) {
	t.Helper()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.mu.Lock()
			p.pods[key].Annotations[annotationPostCloneState] = postCloneStateDone
			p.mu.Unlock()
		}
	})
	t.Cleanup(func() {
		close(stop)
		wg.Wait()
	})
}

func newHibernateFixture(t *testing.T, rt *fakeRuntime, vmID, ip string) (*Provider, *corev1.Pod) {
	t.Helper()
	p := newTestProvider(t)
	p.Runtime = rt
	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSLinux),
	})
	pod.Annotations[meta.AnnotationVMID] = vmID
	pod.Annotations[meta.AnnotationIP] = ip
	return p, pod
}

func newDropNICWakeFixture(t *testing.T, budget, interval time.Duration) (*Provider, *corev1.Pod, *vm.VM) {
	t.Helper()
	p := newTestProvider(t)
	p.Runtime = &fakeRuntime{}
	p.wakeFreshIPBudget = budget
	p.wakeFreshIPInterval = interval

	pod := newPodWithSpec(meta.VMSpec{
		VMName:  "vk-ns-demo-0",
		Backend: string(cocoonv1.BackendCloudHypervisor),
		OS:      string(cocoonv1.OSWindows),
	})
	v := &vm.VM{ID: "vmid-wake", Name: "vk-ns-demo-0"}
	p.trackPod(pod, v)
	p.markLifecycleState(t.Context(), pod, meta.LifecycleStateCreating, "")
	return p, pod, v
}

func newWakeClientsetFixture(t *testing.T) (*Provider, *corev1.Pod, *vm.VM, *fake.Clientset) {
	t.Helper()
	p, pod, v := newDropNICWakeFixture(t, 2*time.Second, 1*time.Millisecond)
	p.Probes.Set(meta.PodKey("ns", "demo-0"), probes.Result{Ready: true})
	client := fake.NewSimpleClientset(pod)
	p.Clientset = client
	return p, pod, v, client
}

func execArgvs(rt *fakeRuntime) []string {
	var out []string
	for _, c := range rt.execCalls {
		out = append(out, strings.Join(c.argv, " "))
	}
	return out
}
