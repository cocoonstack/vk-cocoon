package cocoon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/cocoonstack/cocoon-common/manifest"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/oci"
	"github.com/cocoonstack/cocoon-common/ociutil"
	"github.com/cocoonstack/vk-cocoon/network"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/snapshots"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestCreatePodMissingVMNameRejected(t *testing.T) {
	p := newTestProvider(t)
	p.Runtime = &fakeRuntime{}

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
	p := newTestProvider(t)
	p.Runtime = rt

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
	if len(rt.ensuredImages) != 0 {
		t.Errorf("EnsureImage should not run for an http(s) base image (cocoon --pull handles it), got %#v", rt.ensuredImages)
	}

	p.Close()
	runtime := meta.ParseVMRuntime(pod)
	if runtime.VMID == "" {
		t.Errorf("VMID annotation was not written back")
	}
}

func TestCreatePodCloneModeEnsuresOCIRefBaseImage(t *testing.T) {
	rt := &fakeRuntime{
		snapshots: map[string]*vm.Snapshot{
			"snapshot-repo": {
				Name:  "snapshot-repo",
				Image: "simular/win11:25h2-20260625-1",
			},
		},
	}
	p := newTestProvider(t)
	p.Runtime = rt

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
	if len(rt.ensuredImages) != 1 || rt.ensuredImages[0].image != "simular/win11:25h2-20260625-1" {
		t.Fatalf("OCI-ref base image was not ensured before clone, got %#v", rt.ensuredImages)
	}
	p.Close()
}

func TestCreatePodCloneModeSkipsEnsureWhenDigestPresent(t *testing.T) {
	rt := &fakeRuntime{
		snapshots: map[string]*vm.Snapshot{
			"snapshot-repo": {
				Name:        "snapshot-repo",
				Image:       "simular/win11:25h2-20260625-1",
				ImageDigest: "sha256:7b850cd2",
			},
		},
		imagesPresent: map[string]bool{"sha256:7b850cd2": true},
	}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "snapshot-repo:latest",
		Mode:   "clone",
	})
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(rt.ensuredImages) != 0 {
		t.Fatalf("EnsureImage should be skipped when the digest is already local, got %#v", rt.ensuredImages)
	}
	p.Close()
}

func TestCreatePodForkFromLocalVMSkipsSnapshotBaseImage(t *testing.T) {
	rt := &fakeRuntime{inspectVM: &vm.VM{ID: "source-vm-id", Name: "vk-ns-demo-0"}}
	p := newTestProvider(t)
	p.Runtime = rt

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

func TestEnsureForkSnapshotDedupsConcurrentSaves(t *testing.T) {
	const n = 6
	entered := make(chan struct{})
	closeEntered := sync.OnceFunc(func() { close(entered) })
	release := make(chan struct{})
	rt := &fakeRuntime{snapshotSaveHook: func() {
		closeEntered()
		<-release
	}}
	p := &Provider{
		Runtime:   rt,
		vmsByName: map[string]*vm.VM{"vk-ns-demo-0": {ID: "src", Name: "vk-ns-demo-0"}},
	}

	var wg sync.WaitGroup
	names := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			names[i], errs[i] = p.ensureForkSnapshot(t.Context(), "vk-ns-demo-0")
		})
	}
	<-entered
	close(release)
	wg.Wait()

	if rt.snapshotSaveCount != 1 {
		t.Errorf("concurrent forks saved the shared snapshot %d times, want 1", rt.snapshotSaveCount)
	}
	want := forkSnapshotName("vk-ns-demo-0")
	for i := range n {
		if errs[i] != nil {
			t.Errorf("caller %d: %v", i, errs[i])
		}
		if names[i] != want {
			t.Errorf("caller %d = %q, want %q", i, names[i], want)
		}
	}
}

func TestCreatePodInvalidatesForkSnapshot(t *testing.T) {
	tests := []struct {
		name string
		rt   *fakeRuntime
		spec meta.VMSpec
	}{
		{
			name: "run mode",
			rt:   &fakeRuntime{runVM: &vm.VM{ID: "vmid-main", Name: "vk-ns-demo-0"}},
			spec: meta.VMSpec{VMName: "vk-ns-demo-0", Image: "registry.example/cocoon/ubuntu:24.04", Mode: "run", OS: "linux"},
		},
		{
			name: "clone mode",
			rt:   &fakeRuntime{snapshots: map[string]*vm.Snapshot{"snapshot-repo": {Name: "snapshot-repo"}}},
			spec: meta.VMSpec{VMName: "vk-ns-demo-0", Image: "snapshot-repo:latest", Mode: "clone"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestProvider(t)
			p.Runtime = tt.rt

			pod := newPodWithSpec(tt.spec)
			if err := p.CreatePod(t.Context(), pod); err != nil {
				t.Fatalf("create: %v", err)
			}
			if len(tt.rt.snapshotRemoveCalls) != 1 || tt.rt.snapshotRemoveCalls[0] != "fork-vk-ns-demo-0" {
				t.Fatalf("SnapshotRemoveIfExists calls = %v, want [fork-vk-ns-demo-0]", tt.rt.snapshotRemoveCalls)
			}
		})
	}
}

func TestCreatePodClaimsIncarnationBeforeBringUp(t *testing.T) {
	p := newTestProvider(t)

	podB := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "registry.example/cocoon/ubuntu:24.04",
		Mode:   "run",
		OS:     "linux",
	})
	podB.UID = "b"
	podA := podB.DeepCopy()
	podA.UID = "a"

	rt := &fakeRuntime{
		runVM: &vm.VM{ID: "vmid-main", Name: "vk-ns-demo-0"},
		runHook: func() {
			p.markLifecycleState(t.Context(), podA, meta.LifecycleStateFailed, "stale predecessor")
		},
	}
	p.Runtime = rt

	if err := p.CreatePod(t.Context(), podB); err != nil {
		t.Fatalf("create: %v", err)
	}
	p.mu.RLock()
	got := p.lifecycleIntent[meta.PodKey("ns", "demo-0")]
	p.mu.RUnlock()
	if got.State != meta.LifecycleStateReady {
		t.Errorf("intent state = %q, want ready (stale predecessor write mid-bring-up must drop)", got.State)
	}
}

func TestCreatePodBringUpFailureAllowsRetry(t *testing.T) {
	rt := &fakeRuntime{runErr: errors.New("boot failed")}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "registry.example/cocoon/ubuntu:24.04",
		Mode:   "run",
		OS:     "linux",
	})
	if err := p.CreatePod(t.Context(), pod); err == nil {
		t.Fatal("first create should fail")
	}
	if _, err := p.GetPod(t.Context(), "ns", "demo-0"); err == nil {
		t.Fatal("failed create must not leave a provisional pod, or the kubelet retry becomes UpdatePod")
	}

	rt.runErr = nil
	rt.runVM = &vm.VM{ID: "vmid-main", Name: "vk-ns-demo-0"}
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("retry create: %v", err)
	}
	p.mu.RLock()
	got := p.lifecycleIntent[meta.PodKey("ns", "demo-0")]
	p.mu.RUnlock()
	if got.State != meta.LifecycleStateReady {
		t.Errorf("intent after retry = %q, want ready", got.State)
	}
}

func TestCreatePodForkFromReusesExistingSnapshot(t *testing.T) {
	rt := &fakeRuntime{
		inspectVM: &vm.VM{ID: "source-vm-id", Name: "vk-ns-demo-0"},
		snapshots: map[string]*vm.Snapshot{
			"fork-vk-ns-demo-0": {Name: "fork-vk-ns-demo-0"},
		},
	}
	p := newTestProvider(t)
	p.Runtime = rt

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
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName:   "vk-ns-demo-2",
		Image:    "https://registry.example.org/windows/win11",
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

func TestCreatePodCloneFromDirAnnotationDispatches(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "ignored-when-from-dir-set",
		Mode:   "clone",
	})
	pod.Annotations[meta.AnnotationCloneFromDir] = "/var/lib/cocoon/snaps/foo"

	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.cloned == nil {
		t.Fatalf("Runtime.Clone was not called")
	}
	if rt.cloned.FromDir != "/var/lib/cocoon/snaps/foo" {
		t.Errorf("clone FromDir = %q, want /var/lib/cocoon/snaps/foo", rt.cloned.FromDir)
	}
	if rt.cloned.From != "" {
		t.Errorf("clone From = %q, want empty when FromDir is set", rt.cloned.From)
	}
	if rt.cloned.RestoreMode != vm.RestoreMmap {
		t.Errorf("RestoreMode = %q, want %q on clone-from-dir path", rt.cloned.RestoreMode, vm.RestoreMmap)
	}
	if rt.snapshotSaveCount != 0 || len(rt.ensuredImages) != 0 {
		t.Errorf("from-dir path should bypass snapshot/ensure: save=%d images=%v",
			rt.snapshotSaveCount, rt.ensuredImages)
	}
}

func TestCreatePodCloneFromDirAnnotationConflictsWithRunMode(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "ubuntu-22.04",
		Mode:   "run",
	})
	pod.Annotations[meta.AnnotationCloneFromDir] = "/snaps/foo"

	if err := p.CreatePod(t.Context(), pod); err == nil {
		t.Errorf("expected fast-fail when clone-from-dir is set with mode=run")
	}
	if rt.cloned != nil || rt.ran != nil {
		t.Errorf("no Runtime calls expected on conflict; cloned=%v ran=%v", rt.cloned, rt.ran)
	}
}

func TestCreatePodCloneFromDirAnnotationConflictsWithForkFrom(t *testing.T) {
	rt := &fakeRuntime{inspectVM: &vm.VM{ID: "source-vm-id", Name: "vk-ns-demo-0"}}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName:   "vk-ns-demo-1",
		Image:    "snap:latest",
		Mode:     "clone",
		ForkFrom: "vk-ns-demo-0",
	})
	pod.Annotations[meta.AnnotationCloneFromDir] = "/snaps/foo"

	if err := p.CreatePod(t.Context(), pod); err == nil {
		t.Errorf("expected fast-fail when clone-from-dir is set with fork-from")
	}
	if rt.cloned != nil {
		t.Errorf("no clone call expected on conflict, got %#v", rt.cloned)
	}
}

func TestCreatePodCloneFromDirAnnotationRejectsRelativePath(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "ignored",
		Mode:   "clone",
	})
	pod.Annotations[meta.AnnotationCloneFromDir] = "relative/path"

	if err := p.CreatePod(t.Context(), pod); err == nil {
		t.Errorf("expected error for relative path in annotation")
	}
}

func TestCreatePodCloneFromDirAnnotationRejectsNonCanonicalPath(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "ignored",
		Mode:   "clone",
	})
	pod.Annotations[meta.AnnotationCloneFromDir] = "/var/lib/cocoon/../etc"

	if err := p.CreatePod(t.Context(), pod); err == nil {
		t.Errorf("expected error for non-canonical path containing ..")
	}
	if rt.cloned != nil {
		t.Errorf("Runtime.Clone should not be invoked when path validation fails")
	}
}

func TestCreatePodCloneFromDirAnnotationWhitespaceTreatedAsAbsent(t *testing.T) {
	rt := &fakeRuntime{
		snapshots: map[string]*vm.Snapshot{
			"snap-x": {Name: "snap-x"},
		},
	}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "snap-x:latest",
		Mode:   "clone",
	})
	pod.Annotations[meta.AnnotationCloneFromDir] = "   \t  "

	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.cloned == nil {
		t.Fatalf("Runtime.Clone was not called")
	}
	if rt.cloned.FromDir != "" {
		t.Errorf("FromDir = %q, want empty (whitespace-only annotation must fall through to default clone)", rt.cloned.FromDir)
	}
	if rt.cloned.From != "snap-x" {
		t.Errorf("From = %q, want snap-x (default clone path)", rt.cloned.From)
	}
}

func TestCreatePodCloneFromDirRuntimeFailureSurfacesError(t *testing.T) {
	rt := &fakeRuntime{cloneErr: errors.New("ch boot rejected")}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-demo-0",
		Image:  "ignored",
		Mode:   "clone",
	})
	pod.Annotations[meta.AnnotationCloneFromDir] = "/var/lib/cocoon/snaps/foo"

	err := p.CreatePod(t.Context(), pod)
	if err == nil {
		t.Fatalf("expected error to surface from Runtime.Clone failure")
	}
	if !strings.Contains(err.Error(), "ch boot rejected") {
		t.Errorf("error chain missing root cause: %v", err)
	}
	if !strings.Contains(err.Error(), "/var/lib/cocoon/snaps/foo") {
		t.Errorf("error should name the dir for ops triage: %v", err)
	}
}

func TestCreatePodRunMode(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

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
	if rt.ran.Memory != "4Gi" {
		t.Fatalf("Run Memory = %q, want 4Gi (vm layer owns the byte conversion)", rt.ran.Memory)
	}
	if len(pod.Status.ContainerStatuses) != 1 || !pod.Status.ContainerStatuses[0].Ready {
		t.Fatalf("pod status was not refreshed to Ready: %#v", pod.Status.ContainerStatuses)
	}
}

func TestCreatePodRunModeBurstableCPUPolicy(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-burst",
		Image:  "ubuntu-22.04",
		Mode:   "run",
	})
	pod.Spec.Containers = []corev1.Container{{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		},
	}}
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.ran == nil {
		t.Fatal("Runtime.Run was not called")
	}
	if rt.ran.CPU != 2 {
		t.Fatalf("Run CPU = %d, want 2 (vCPU bounds at the limit, not the request)", rt.ran.CPU)
	}
	if rt.ran.CPUWeight != 20 || rt.ran.CPUQuotaUs != 200000 {
		t.Fatalf("Run CPU policy = weight %d quota %d, want 20/200000", rt.ran.CPUWeight, rt.ran.CPUQuotaUs)
	}
}

func TestCreatePodCloneModePassesCPUPolicy(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-clone-policy",
		Image:  "golden",
	})
	pod.Spec.Containers = []corev1.Container{{
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m")},
		},
	}}
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	if rt.cloned == nil {
		t.Fatal("Runtime.Clone was not called")
	}
	if rt.cloned.CPUWeight != 39 || rt.cloned.CPUQuotaUs != 150000 {
		t.Fatalf("Clone CPU policy = weight %d quota %d, want 39/150000", rt.cloned.CPUWeight, rt.cloned.CPUQuotaUs)
	}
}

func TestPodCPUPolicy(t *testing.T) {
	tests := []struct {
		name      string
		resources corev1.ResourceRequirements
		want      vm.CPUPolicy
	}{
		{
			name: "burstable derives quota from limits and weight from requests",
			resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			},
			want: vm.CPUPolicy{CPUWeight: 20, CPUQuotaUs: 200000, CPUPeriodUs: 100000},
		},
		{
			name: "limits only fall back to limits for weight",
			resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			},
			want: vm.CPUPolicy{CPUWeight: 39, CPUQuotaUs: 100000, CPUPeriodUs: 100000},
		},
		{
			name: "requests only leave quota at cocoon defaults",
			resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
			},
			want: vm.CPUPolicy{CPUWeight: 79},
		},
		{
			name: "sub-10m limit clamps quota to the kernel floor",
			resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5m")},
			},
			want: vm.CPUPolicy{CPUWeight: 1, CPUQuotaUs: 1000, CPUPeriodUs: 100000},
		},
		{
			name: "no cpu resources get kubelet's BestEffort minimum weight",
			want: vm.CPUPolicy{CPUWeight: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-policy"})
			pod.Spec.Containers = []corev1.Container{{Resources: tt.resources}}
			if policy := podCPUPolicy(pod); policy != tt.want {
				t.Errorf("podCPUPolicy = %+v, want %+v", policy, tt.want)
			}
		})
	}
}

func TestCPUWeightFromMilliBounds(t *testing.T) {
	if got := cpuWeightFromMilli(1); got != 1 {
		t.Errorf("cpuWeightFromMilli(1) = %d, want 1 (min clamp)", got)
	}
	if got := cpuWeightFromMilli(300000); got != 10000 {
		t.Errorf("cpuWeightFromMilli(300000) = %d, want 10000 (max clamp)", got)
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

func TestLocalSnapshotNameBoundsLongRegistryRefs(t *testing.T) {
	repo := "simular/ubuntu2404-base-snapshot"
	tag := "ubuntu2404-20260716-29479663055-1"

	got := localSnapshotName(repo, tag)
	if len(got) > 63 {
		t.Fatalf("local snapshot name length = %d, want <= 63: %q", len(got), got)
	}
	if got != localSnapshotName(repo, tag) {
		t.Fatal("local snapshot name is not deterministic")
	}
	if got == localSnapshotName(repo, tag+"-different") {
		t.Fatal("different registry refs produced the same local snapshot name")
	}
	if !strings.HasPrefix(got, "simular/ubuntu2404-base-snapshot:") {
		t.Fatalf("local snapshot name lost its recognizable prefix: %q", got)
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
	p := newTestProvider(t)
	p.Runtime = rt

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
	p := newTestProvider(t)
	p.Runtime = rt

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
	p := newTestProvider(t)
	p.Runtime = rt

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
	p := newTestProvider(t)
	p.Runtime = rt

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
	p := newTestProvider(t)
	p.Runtime = rt

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
	p := newTestProvider(t)
	p.Runtime = rt

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

func TestEnsureRunImageFallback(t *testing.T) {
	cases := []struct {
		name    string
		image   string
		wantArg string
	}{
		{name: "empty skipped", image: "", wantArg: ""},
		{name: "https url", image: "https://cloud-images.ubuntu.com/x.img", wantArg: "https://cloud-images.ubuntu.com/x.img"},
		{name: "http url", image: "http://x/y.img", wantArg: "http://x/y.img"},
		{name: "no puller falls through", image: "ubuntu-22.04", wantArg: "ubuntu-22.04"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeRuntime{}
			p := newTestProvider(t)
			p.Runtime = rt
			got, err := p.ensureRunImage(t.Context(), tc.image, false)
			if err != nil {
				t.Fatalf("ensureRunImage: %v", err)
			}
			if got != tc.image {
				t.Fatalf("ensureRunImage returned %q, want %q (input unchanged on fallback paths)", got, tc.image)
			}
			if tc.wantArg == "" {
				if len(rt.ensuredImages) != 0 {
					t.Fatalf("EnsureImage should not run for empty ref, got %v", rt.ensuredImages)
				}
				return
			}
			if len(rt.ensuredImages) != 1 || rt.ensuredImages[0].image != tc.wantArg {
				t.Fatalf("EnsureImage called with %v, want exactly one call for %q", rt.ensuredImages, tc.wantArg)
			}
		})
	}
}

func TestEnsureRunImageDispatch(t *testing.T) {
	const repo = "ubuntu"

	cases := []struct {
		name           string
		manifestStatus int
		manifestBody   string
		imagesPresent  bool
		wantErr        string
		wantPullerHit  bool
		wantEnsureArg  string
	}{
		{
			name:           "cloud image dispatched to Puller",
			manifestStatus: http.StatusOK,
			manifestBody:   `{"artifactType":"application/vnd.cocoonstack.os-image.v1+json"}`,
			imagesPresent:  true,
			wantPullerHit:  true,
		},
		{
			name:           "snapshot artifact rejected",
			manifestStatus: http.StatusOK,
			manifestBody:   `{"artifactType":"application/vnd.cocoonstack.snapshot.v1+json"}`,
			wantErr:        "snapshot artifact",
		},
		{
			name:           "classify-error falls through",
			manifestStatus: http.StatusOK,
			manifestBody:   `not-json`,
			wantEnsureArg:  repo,
		},
		{
			name:           "GetManifest 5xx falls through",
			manifestStatus: http.StatusInternalServerError,
			manifestBody:   "boom",
			wantEnsureArg:  repo,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := fakeRegistry{manifest: []byte(tc.manifestBody)}
			if tc.manifestStatus/100 != 2 {
				reg.err = errors.New("registry error")
			}
			wantRef := repo + ":latest"
			rt := &fakeRuntime{}
			if tc.imagesPresent {
				rt.imagesPresent = map[string]bool{wantRef: true}
			}
			p := newTestProvider(t)
			p.Runtime = rt
			p.Puller = &snapshots.Puller{Registry: reg, Runtime: rt}

			got, err := p.ensureRunImage(t.Context(), repo, false)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				if len(rt.ensuredImages) != 0 {
					t.Fatalf("EnsureImage should not run on snapshot-reject path, got %v", rt.ensuredImages)
				}
			case tc.wantPullerHit:
				if err != nil {
					t.Fatalf("ensureRunImage: %v", err)
				}
				if len(rt.imageInspectCalls) == 0 {
					t.Fatalf("expected Image() inspect on Puller path, got none")
				}
				if len(rt.ensuredImages) != 0 {
					t.Fatalf("Puller path should not shell EnsureImage, got %v", rt.ensuredImages)
				}
				if got != wantRef {
					t.Fatalf("ensureRunImage returned %q, want %q", got, wantRef)
				}
				if len(rt.imageInspectCalls) == 0 || rt.imageInspectCalls[0] != wantRef {
					t.Fatalf("Puller imported under %v, want first inspect on %q", rt.imageInspectCalls, wantRef)
				}
			default:
				if err != nil {
					t.Fatalf("ensureRunImage: %v", err)
				}
				if got != repo {
					t.Fatalf("ensureRunImage returned %q, want %q (fallback returns input unchanged)", got, repo)
				}
				if len(rt.ensuredImages) != 1 || rt.ensuredImages[0].image != tc.wantEnsureArg {
					t.Fatalf("EnsureImage = %v, want one call for %q", rt.ensuredImages, tc.wantEnsureArg)
				}
			}
		})
	}
}

func TestEnsureSnapshotColdBurstSingleflight(t *testing.T) {
	p, rt, reg := newSnapshotTestSetup(t)

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	snaps := make([]*vm.Snapshot, n)
	for i := range n {
		wg.Go(func() {
			snaps[i], errs[i] = p.ensureSnapshot(t.Context(), "ubuntu", "v1", "ubuntu:v1")
		})
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if snaps[i] == nil || snaps[i].Name != "ubuntu:v1" {
			t.Fatalf("caller %d snapshot = %+v, want ubuntu:v1", i, snaps[i])
		}
	}
	if len(rt.snapshotImports) != 1 {
		t.Errorf("snapshot imports = %v, want exactly one", rt.snapshotImports)
	}
	if got := reg.manifests.Load(); got != 1 {
		t.Errorf("manifest fetches = %d, want 1", got)
	}
}

func TestEnsureSnapshotLocalHitSkipsRegistry(t *testing.T) {
	p, rt, reg := newSnapshotTestSetup(t)
	rt.registerSnapshot("ubuntu:v1")

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = p.ensureSnapshot(t.Context(), "ubuntu", "v1", "ubuntu:v1")
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if len(rt.snapshotImports) != 0 || reg.manifests.Load() != 0 {
		t.Errorf("local hit performed transfer/import: imports=%v manifests=%d",
			rt.snapshotImports, reg.manifests.Load())
	}
}

func TestEnsureSnapshotDifferentRefsNotSerialized(t *testing.T) {
	p, rt, _ := newSnapshotTestSetup(t)
	both := make(chan struct{})
	var entered atomic.Int64
	rt.importHook = func() {
		if entered.Add(1) == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(5 * time.Second):
			t.Error("second flight never imported while the first held its import: flights serialized")
		}
	}

	var wg sync.WaitGroup
	var errA, errB error
	wg.Go(func() { _, errA = p.ensureSnapshot(t.Context(), "ubuntu", "v1", "ubuntu:v1") })
	wg.Go(func() { _, errB = p.ensureSnapshot(t.Context(), "debian", "v2", "debian:v2") })
	wg.Wait()
	if errA != nil || errB != nil {
		t.Fatalf("errA=%v errB=%v", errA, errB)
	}
	if len(rt.snapshotImports) != 2 {
		t.Errorf("imports = %v, want one per ref", rt.snapshotImports)
	}
}

func TestEnsureSnapshotSharedWorkSurvivesCallerCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, rt, _ := newSnapshotTestSetup(t)
		release := make(chan struct{})
		rt.importHook = func() { <-release }

		leaderCtx, cancelLeader := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		var errLeader, errLive error
		var liveSnapshot *vm.Snapshot
		wg.Go(func() { _, errLeader = p.ensureSnapshot(leaderCtx, "ubuntu", "v1", "ubuntu:v1") })
		synctest.Wait()
		wg.Go(func() { liveSnapshot, errLive = p.ensureSnapshot(t.Context(), "ubuntu", "v1", "ubuntu:v1") })
		synctest.Wait()
		cancelLeader()
		synctest.Wait()
		close(release)
		wg.Wait()

		if !errors.Is(errLeader, context.Canceled) {
			t.Errorf("cancelled caller err = %v, want context.Canceled (it must not wait out the shared import)", errLeader)
		}
		if errLive != nil || liveSnapshot == nil {
			t.Fatalf("shared work must outlive the cancelled caller: snapshot=%v err=%v", liveSnapshot, errLive)
		}
		if len(rt.snapshotImports) != 1 {
			t.Errorf("imports = %v, want one", rt.snapshotImports)
		}
	})
}

func TestEnsureSnapshotSharedImportAbortedByClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, rt, _ := newSnapshotTestSetup(t)
		release := make(chan struct{})
		rt.importHook = func() { <-release }

		var wg sync.WaitGroup
		var err error
		wg.Go(func() { _, err = p.ensureSnapshot(t.Context(), "ubuntu", "v1", "ubuntu:v1") })
		synctest.Wait()
		p.Close()
		close(release)
		wg.Wait()

		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled: provider shutdown must abort the detached import", err)
		}
	})
}

func TestEnsureSnapshotRetryAfterSharedFailure(t *testing.T) {
	p, rt, reg := newSnapshotTestSetup(t)
	reg.err = errors.New("registry down")

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = p.ensureSnapshot(t.Context(), "ubuntu", "v1", "ubuntu:v1")
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller %d: want shared failure, got nil", i)
		}
	}
	if len(rt.snapshotImports) != 0 {
		t.Fatalf("failed manifest fetch must not reach import, got %v", rt.snapshotImports)
	}

	reg.err = nil
	snapshot, err := p.ensureSnapshot(t.Context(), "ubuntu", "v1", "ubuntu:v1")
	if err != nil || snapshot == nil {
		t.Fatalf("retry after shared failure: snapshot=%v err=%v", snapshot, err)
	}
	if len(rt.snapshotImports) != 1 {
		t.Errorf("imports = %v, want one after retry", rt.snapshotImports)
	}
}

func TestEnsureRunImageColdBurstSingleImport(t *testing.T) {
	p, rt, _ := newCloudImageTestSetup(t)

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	refs := make([]string, n)
	for i := range n {
		wg.Go(func() {
			refs[i], errs[i] = p.ensureRunImage(t.Context(), "ubuntu", false)
		})
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if refs[i] != "ubuntu:latest" {
			t.Fatalf("caller %d ref = %q, want ubuntu:latest", i, refs[i])
		}
	}
	if len(rt.imageImports) != 1 {
		t.Errorf("image imports = %v, want exactly one", rt.imageImports)
	}
}

func TestEnsureRunImageWarmHitNoImport(t *testing.T) {
	p, rt, _ := newCloudImageTestSetup(t)
	rt.imagesPresent = map[string]bool{"ubuntu:latest": true}

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = p.ensureRunImage(t.Context(), "ubuntu", false)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if len(rt.imageImports) != 0 {
		t.Errorf("warm hit imported anyway: %v", rt.imageImports)
	}
}

func TestEnsureRunImageLaterForceReimports(t *testing.T) {
	p, rt, _ := newCloudImageTestSetup(t)

	for i, force := range []bool{false, false, true} {
		if _, err := p.ensureRunImage(t.Context(), "ubuntu", force); err != nil {
			t.Fatalf("call %d (force=%v): %v", i, force, err)
		}
	}
	if len(rt.imageImports) != 2 {
		t.Errorf("imports = %v, want cold import + warm hit + later-force reimport", rt.imageImports)
	}
}

func TestEnsureRunImageConcurrentForceShareOneImport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, rt, _ := newCloudImageTestSetup(t)
		entered := make(chan struct{})
		closeEntered := sync.OnceFunc(func() { close(entered) })
		release := make(chan struct{})
		rt.importHook = func() { closeEntered(); <-release }

		var wg sync.WaitGroup
		var err1, err2 error
		wg.Go(func() { _, err1 = p.ensureRunImage(t.Context(), "ubuntu", true) })
		<-entered
		wg.Go(func() { _, err2 = p.ensureRunImage(t.Context(), "ubuntu", true) })
		synctest.Wait()
		close(release)
		wg.Wait()

		if err1 != nil || err2 != nil {
			t.Fatalf("err1=%v err2=%v", err1, err2)
		}
		if len(rt.imageImports) != 1 {
			t.Errorf("concurrent force imports = %v, want one shared", rt.imageImports)
		}
	})
}

func TestEnsureRunImageFallbackDeduped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := &fakeRuntime{}
		reg := &countingRegistry{fakeRegistry: fakeRegistry{err: errors.New("registry down")}}
		p := newTestProvider(t)
		p.Runtime = rt
		p.Puller = &snapshots.Puller{Registry: reg, Runtime: rt}

		entered := make(chan struct{})
		closeEntered := sync.OnceFunc(func() { close(entered) })
		release := make(chan struct{})
		rt.ensureImageHook = func() { closeEntered(); <-release }

		var wg sync.WaitGroup
		var err1, err2 error
		wg.Go(func() { _, err1 = p.ensureRunImage(t.Context(), "ubuntu-22.04", false) })
		<-entered
		wg.Go(func() { _, err2 = p.ensureRunImage(t.Context(), "ubuntu-22.04", false) })
		synctest.Wait()
		close(release)
		wg.Wait()

		if err1 != nil || err2 != nil {
			t.Fatalf("err1=%v err2=%v", err1, err2)
		}
		if len(rt.ensuredImages) != 1 {
			t.Errorf("fallback EnsureImage calls = %v, want one shared", rt.ensuredImages)
		}
	})
}

func TestDeletePodRemovesAndForgetsVM(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := newPodWithSpec(meta.VMSpec{
		VMName:         "vk-ns-demo-0",
		SnapshotPolicy: "never",
	})
	p.trackPod(pod, &vm.VM{ID: "vmid-del", Name: "vk-ns-demo-0"})
	p.Probes.Set(meta.PodKey("ns", "demo-0"), probes.Result{Ready: true})

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
	gotSnapRemovals := slices.Sorted(slices.Values(rt.snapshotRemoveCalls))
	wantSnapRemovals := slices.Sorted(slices.Values([]string{"vk-ns-demo-0", forkSnapshotName("vk-ns-demo-0")}))
	if !slices.Equal(gotSnapRemovals, wantSnapRemovals) {
		t.Errorf("snapshot removes = %v, want %v", gotSnapRemovals, wantSnapRemovals)
	}
}

func TestCreatePodUnmanagedAdoptsExistingVM(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

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
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
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
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()
	p.OrphanPolicy = provider.OrphanDestroy

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if rt.removedID != "orphan-vmid" {
		t.Errorf("orphan VM should be removed under OrphanDestroy, removedID=%q", rt.removedID)
	}
}

func TestStartupReconcileOrphanAlertIndexesByName(t *testing.T) {
	rt := &fakeRuntime{
		listVMs: []vm.VM{{ID: "live-vmid", Name: "vk-ns-demo-0", IP: "10.0.0.42"}},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()
	p.OrphanPolicy = provider.OrphanAlert

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if got := p.vmByName("vk-ns-demo-0"); got == nil || got.ID != "live-vmid" {
		t.Fatalf("orphan VM must be indexed by name so CreatePod can adopt it, got %#v", got)
	}
	if rt.removedID != "" {
		t.Errorf("OrphanAlert must not remove the VM, removedID=%q", rt.removedID)
	}
}

func TestStartupReconcileOrphanKeepIndexesByName(t *testing.T) {
	rt := &fakeRuntime{
		listVMs: []vm.VM{{ID: "live-vmid", Name: "vk-ns-demo-0", IP: "10.0.0.42"}},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()
	p.OrphanPolicy = provider.OrphanKeep

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if got := p.vmByName("vk-ns-demo-0"); got == nil || got.ID != "live-vmid" {
		t.Fatalf("OrphanKeep must index by name, got %#v", got)
	}
}

func TestStartupReconcileAdoptsByVMNameWhenAnnotationMissing(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	pod.Spec.NodeName = "cocoon-pool"

	rt := &fakeRuntime{
		listVMs: []vm.VM{{ID: "live-vmid", Name: "vk-ns-demo-0", IP: "10.0.0.42"}},
	}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset(pod)
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
	meta.HibernateState(true).Apply(pod)

	rt := &fakeRuntime{listVMs: nil}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset(pod)

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	if _, err := p.GetPod(t.Context(), "ns", "demo-0"); err != nil {
		t.Errorf("hibernated pod should be tracked: %v", err)
	}
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

	p := newTestProvider(t)
	p.Runtime = &fakeRuntime{}
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
	cs := fake.NewSimpleClientset()

	p := newTestProvider(t)
	p.Runtime = &fakeRuntime{}
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

func TestHandleVMGoneReleasesTrackedLeaseForSparseEvent(t *testing.T) {
	releaser := &recordingLeaseReleaser{}
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p := newTestProvider(t)
	p.Runtime = &fakeRuntime{inspectErr: fmt.Errorf("inspect: %w", vm.ErrVMNotFound)}
	p.Clientset = fake.NewSimpleClientset(pod)
	p.LeaseReleaser = releaser
	p.trackPod(pod, &vm.VM{ID: "vmid-g", Name: "vk-ns-demo-0", MAC: "aa:bb:cc:dd:ee:ff"})

	p.handleVMGone(t.Context(), &vm.VM{ID: "vmid-g"})

	if got := releaser.awaitReleases(t, 1); !slices.Equal(got, []string{"aa:bb:cc:dd:ee:ff"}) {
		t.Fatalf("released MACs = %v, want tracked VM MAC", got)
	}
	if got := p.vmForPod("ns", "demo-0"); got != nil {
		t.Fatalf("gone VM should be evicted, still tracked as %#v", got)
	}
}

func TestHandleVMGoneSkippedWhenPodHibernating(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()

	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	meta.HibernateState(true).Apply(pod)
	p.trackPod(pod, &vm.VM{ID: "vmid-h", Name: "vk-ns-demo-0"})

	p.handleVMGone(t.Context(), &vm.VM{ID: "vmid-h", Name: "vk-ns-demo-0"})

	if rt.inspectN != 0 {
		t.Errorf("inspect must not be called for hibernating pod, got %d calls", rt.inspectN)
	}
	if got := p.vmForPod("ns", "demo-0"); got == nil {
		t.Errorf("hibernating pod's VM tracking must be preserved")
	}
}

func TestHandleVMGoneSkippedWhenPodDeleting(t *testing.T) {
	rt := &fakeRuntime{inspectVM: &vm.VM{ID: "vmid-d", Name: "vk-ns-demo-0", State: "stopped"}}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()

	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.trackPod(pod, &vm.VM{ID: "vmid-d", Name: "vk-ns-demo-0"})
	p.mu.Lock()
	p.deleting[meta.PodKey("ns", "demo-0")] = struct{}{}
	p.mu.Unlock()

	p.handleVMGone(t.Context(), &vm.VM{ID: "vmid-d", Name: "vk-ns-demo-0"})

	if len(rt.startCalls) != 0 {
		t.Errorf("restart must not run for a pod mid-delete, got %v", rt.startCalls)
	}
	if got := p.vmForPod("ns", "demo-0"); got == nil {
		t.Errorf("mid-delete pod's VM tracking must be preserved for DeletePod's removeVM")
	}
}

func TestHandleVMGoneInlineRetryRecoversFromTransient(t *testing.T) {
	rt := &fakeRuntime{
		inspectSeq: []fakeInspectStep{
			{err: errors.New("exec: broken pipe")},
			{vm: &vm.VM{ID: "vmid-r", Name: "vk-ns-demo-0", State: vm.StateRunning}},
		},
	}
	p := newTestProvider(t)
	p.Runtime = rt
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
	rt := &fakeRuntime{
		inspectSeq: []fakeInspectStep{
			{err: errors.New("exec: broken pipe")},
			{err: errors.New("exec: broken pipe")},
			{err: errors.New("exec: broken pipe")},
			{err: fmt.Errorf("inspect: %w", vm.ErrVMNotFound)},
		},
	}
	p := newTestProvider(t)
	p.Runtime = rt
	releaser := &recordingLeaseReleaser{}
	p.LeaseReleaser = releaser
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.Clientset = fake.NewSimpleClientset(pod)
	p.trackPod(pod, &vm.VM{ID: "vmid-d", Name: "vk-ns-demo-0", MAC: "aa:bb:cc:dd:ee:01"})
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
	if got := releaser.awaitReleases(t, 1); !slices.Equal(got, []string{"aa:bb:cc:dd:ee:01"}) {
		t.Fatalf("released MACs = %v, want tracked VM MAC", got)
	}
}

func TestHandleVMGoneDeferredRecheckHitsBudgetAndEvicts(t *testing.T) {
	rt := &fakeRuntime{inspectErr: errors.New("exec: broken pipe")}
	p := newTestProvider(t)
	p.Runtime = rt
	releaser := &recordingLeaseReleaser{}
	p.LeaseReleaser = releaser
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.Clientset = fake.NewSimpleClientset(pod)
	p.trackPod(pod, &vm.VM{ID: "vmid-b", Name: "vk-ns-demo-0", MAC: "aa:bb:cc:dd:ee:02"})
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
	if rt.removedID != "vmid-b" {
		t.Fatalf("timeout must remove VM before eviction, removed %q", rt.removedID)
	}
	if got := releaser.awaitReleases(t, 1); !slices.Equal(got, []string{"aa:bb:cc:dd:ee:02"}) {
		t.Fatalf("released MACs = %v, want tracked VM MAC", got)
	}
}

func TestHandleVMGoneDeferredTimeoutKeepsLeaseWhenRemovalFails(t *testing.T) {
	rt := &fakeRuntime{inspectErr: errors.New("exec: broken pipe"), removeErr: errors.New("still running")}
	releaser := &recordingLeaseReleaser{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.LeaseReleaser = releaser
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.Clientset = fake.NewSimpleClientset(pod)
	p.trackPod(pod, &vm.VM{ID: "vmid-safe", Name: "vk-ns-demo-0", MAC: "aa:bb:cc:dd:ee:03"})
	p.inlineInspectBaseDelay = time.Millisecond
	p.deferredRecheckInitialDelay = time.Millisecond
	p.deferredRecheckMaxDelay = 2 * time.Millisecond
	p.deferredRecheckBudget = 5 * time.Millisecond

	p.handleVMGone(t.Context(), &vm.VM{ID: "vmid-safe"})
	p.recheckWG.Wait()

	if got := p.vmForPod("ns", "demo-0"); got == nil {
		t.Fatal("pod tracking was evicted even though VM removal failed")
	}
	if got := releaser.released(); len(got) != 0 {
		t.Fatalf("released MACs = %v while VM state remained uncertain", got)
	}
}

func TestHandleVMGoneDeferredRecheckDedups(t *testing.T) {
	rt := &fakeRuntime{inspectErr: errors.New("exec: broken pipe")}
	p := newTestProvider(t)
	p.Runtime = rt
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
	rt := &fakeRuntime{inspectErr: errors.New("exec: broken pipe")}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset()
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.trackPod(pod, &vm.VM{ID: "vmid-close", Name: "vk-ns-demo-0"})
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

	p.scheduleDeferredRecheck("vmid-close")
	p.mu.RLock()
	n := len(p.pendingRecheck)
	p.mu.RUnlock()
	if n != 0 {
		t.Fatalf("scheduleDeferredRecheck after Close should no-op, pendingRecheck=%d", n)
	}
}

func TestGetPodStatusRefreshesIPFromLease(t *testing.T) {
	p := newTestProvider(t)
	p.Probes.Set("ns/demo-0", probes.Result{Ready: true})

	p.LeaseParser = newLeaseParser(t, "aa:bb:cc:dd:ee:ff", "172.20.0.88")

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

func TestCreatePodAdoptPublishesStatusBeforeReady(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	client := fake.NewSimpleClientset(pod)
	p.Clientset = client
	p.trackPod(pod, &vm.VM{ID: "vmid-adopt", Name: "vk-ns-demo-0", IP: "192.0.2.10"})

	var mu sync.Mutex
	var seq []string
	client.PrependReactor("*", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if a, ok := action.(k8stesting.PatchActionImpl); ok {
			switch {
			case a.GetSubresource() == "status" && strings.Contains(string(a.GetPatch()), `"podIP":"192.0.2.10"`):
				seq = append(seq, "status")
			case a.GetSubresource() == "" && strings.Contains(string(a.GetPatch()), string(meta.LifecycleStateReady)):
				seq = append(seq, "ready")
			}
		}
		return false, nil, nil
	})

	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	publishedAt := slices.Index(seq, "status")
	readyAt := slices.Index(seq, "ready")
	if publishedAt < 0 {
		t.Fatalf("status was never published to the apiserver, writes = %v", seq)
	}
	if readyAt < 0 {
		t.Fatalf("ready was never patched, writes = %v", seq)
	}
	if publishedAt > readyAt {
		t.Errorf("ready patched before the status reached the apiserver, writes = %v", seq)
	}
}

type fakeInspectStep struct {
	vm  *vm.VM
	err error
}

type netResizeCall struct {
	vmID   string
	target int
}

type fakeExecCall struct {
	vmID  string
	argv  []string
	env   map[string]string
	stdin string
}

type fakeLogsCall struct {
	vmID string
	tail int
}

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
	runHook             func()
	execCalls           []fakeExecCall
	execStdout          string
	execStderr          string
	execExitCode        int
	execErr             error
	logsCalls           []fakeLogsCall
	logsOut             string
	logsErr             error
	ensuredImages       []struct {
		image string
		force bool
	}
	imagesPresent     map[string]bool
	imageInspectCalls []string

	netResizeCalls      []netResizeCall
	netResizeErr        error
	netResizeNeedsStart bool

	startCalls []string

	mu              sync.Mutex
	snapshotImports []string
	imageImports    []string
	importHook      func()
	ensureImageHook func()

	onRemove func()

	staleCreateOutcomes map[string]vm.StaleCreateOutcome
	staleCreateSeq      map[string][]vm.StaleCreateOutcome
	staleCreateCalls    []string
	staleCreateErr      error
	onExec              func()
	removeErr           error
	snapshotSaveErr     error
	snapshotSaveHook    func()

	inspectMu  sync.Mutex
	inspectSeq []fakeInspectStep
	inspectN   int
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
	if f.runHook != nil {
		f.runHook()
	}
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
	if f.onRemove != nil {
		f.onRemove()
	}
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removedID = vmID
	return nil
}

func (f *fakeRuntime) ReconcileStaleCreate(_ context.Context, vmID string) (vm.StaleCreateOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staleCreateCalls = append(f.staleCreateCalls, vmID)
	if f.staleCreateErr != nil {
		return "", f.staleCreateErr
	}
	if seq := f.staleCreateSeq[vmID]; len(seq) > 0 {
		f.staleCreateSeq[vmID] = seq[1:]
		return seq[0], nil
	}
	if o, ok := f.staleCreateOutcomes[vmID]; ok {
		return o, nil
	}
	return vm.StaleCreateCollected, nil
}

func (f *fakeRuntime) SnapshotSave(_ context.Context, name, vmID string) error {
	if f.snapshotSaveHook != nil {
		f.snapshotSaveHook()
	}
	if f.snapshotSaveErr != nil {
		return f.snapshotSaveErr
	}
	f.savedSnapshot.name = name
	f.savedSnapshot.vmID = vmID
	f.snapshotSaveCount++
	f.registerSnapshot(name)
	return nil
}

func (f *fakeRuntime) SnapshotRemoveIfExists(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotRemoveCalls = append(f.snapshotRemoveCalls, name)
	delete(f.snapshots, name)
	return nil
}

func (f *fakeRuntime) Snapshot(_ context.Context, name string) (*vm.Snapshot, error) {
	if f.snapshotErr != nil {
		return nil, f.snapshotErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapshots != nil {
		if snapshot, ok := f.snapshots[name]; ok {
			return snapshot, nil
		}
	}
	return nil, fmt.Errorf("snapshot %s: %w", name, vm.ErrSnapshotNotFound)
}

func (f *fakeRuntime) SnapshotImport(ctx context.Context, name string) (io.WriteCloser, func() error, error) {
	f.mu.Lock()
	f.snapshotImports = append(f.snapshotImports, name)
	hook := f.importHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	wait := func() error {
		f.registerSnapshot(name)
		return nil
	}
	return nopWriteCloser{}, wait, nil
}

func (f *fakeRuntime) SnapshotExport(_ context.Context, _ string) (io.ReadCloser, func() error, error) {
	return io.NopCloser(nil), func() error { return nil }, nil
}

func (f *fakeRuntime) EnsureImage(_ context.Context, image string, force bool) error {
	f.mu.Lock()
	f.ensuredImages = append(f.ensuredImages, struct {
		image string
		force bool
	}{image, force})
	hook := f.ensureImageHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (f *fakeRuntime) Image(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imageInspectCalls = append(f.imageInspectCalls, name)
	if f.imagesPresent[name] {
		return nil
	}
	return fmt.Errorf("image %s: %w", name, vm.ErrImageNotFound)
}

func (f *fakeRuntime) ImageImport(ctx context.Context, name string) (io.WriteCloser, func() error, error) {
	f.mu.Lock()
	f.imageImports = append(f.imageImports, name)
	hook := f.importHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	wait := func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.imagesPresent == nil {
			f.imagesPresent = map[string]bool{}
		}
		f.imagesPresent[name] = true
		return nil
	}
	return nopWriteCloser{}, wait, nil
}

func (f *fakeRuntime) Start(_ context.Context, vmID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls = append(f.startCalls, vmID)
	return nil
}

func (f *fakeRuntime) NetResize(ctx context.Context, vmID string, target int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.netResizeCalls = append(f.netResizeCalls, netResizeCall{vmID: vmID, target: target})
	if f.netResizeNeedsStart && len(f.started()) == 0 {
		return fmt.Errorf("cocoon vm net %s: vm is not running: vm not running", vmID)
	}
	return f.netResizeErr
}

func (f *fakeRuntime) Exec(ctx context.Context, vmID string, argv []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.onExec != nil {
		f.onExec()
	}
	call := fakeExecCall{vmID: vmID, argv: argv, env: env}
	if stdin != nil {
		buf, _ := io.ReadAll(stdin)
		call.stdin = string(buf)
	}
	f.execCalls = append(f.execCalls, call)
	if f.execErr != nil {
		return f.execErr
	}
	if stdout != nil && f.execStdout != "" {
		_, _ = stdout.Write([]byte(f.execStdout))
	}
	if stderr != nil && f.execStderr != "" {
		_, _ = stderr.Write([]byte(f.execStderr))
	}
	if f.execExitCode != 0 {
		return utilexec.CodeExitError{Err: fmt.Errorf("fake exec: exit %d", f.execExitCode), Code: f.execExitCode}
	}
	return nil
}

func (f *fakeRuntime) Logs(_ context.Context, vmID string, tail int) (io.ReadCloser, error) {
	f.logsCalls = append(f.logsCalls, fakeLogsCall{vmID: vmID, tail: tail})
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	return io.NopCloser(strings.NewReader(f.logsOut)), nil
}

func (f *fakeRuntime) WatchEvents(_ context.Context) (<-chan vm.VMEvent, error) {
	ch := make(chan vm.VMEvent)
	close(ch)
	return ch, nil
}

func (f *fakeRuntime) registerSnapshot(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapshots == nil {
		f.snapshots = map[string]*vm.Snapshot{}
	}
	f.snapshots[name] = &vm.Snapshot{Name: name}
}

func (f *fakeRuntime) staleCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.staleCreateCalls)
}

func (f *fakeRuntime) started() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.startCalls)
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

var _ oci.Registry = fakeRegistry{}

type fakeRegistry struct {
	manifest []byte
	err      error
}

func (f fakeRegistry) GetManifest(context.Context, string, string) ([]byte, string, error) {
	return f.manifest, "", f.err
}

func (fakeRegistry) GetBlob(context.Context, string, string) (io.ReadCloser, error) { return nil, nil }

func (fakeRegistry) HasBlob(context.Context, string, string) (bool, error) { return false, nil }

func (fakeRegistry) PutBlob(context.Context, string, string, io.Reader, int64) error { return nil }

func (fakeRegistry) PutManifest(context.Context, string, string, []byte, string) error { return nil }

func (fakeRegistry) HasManifest(context.Context, string, string) (bool, error) { return false, nil }

func (fakeRegistry) DeleteManifest(context.Context, string, string) error { return nil }

type countingRegistry struct {
	fakeRegistry
	blobs     map[string][]byte
	manifests atomic.Int64
}

func (c *countingRegistry) GetManifest(ctx context.Context, name, tag string) ([]byte, string, error) {
	c.manifests.Add(1)
	return c.fakeRegistry.GetManifest(ctx, name, tag)
}

func (c *countingRegistry) GetBlob(_ context.Context, _, digest string) (io.ReadCloser, error) {
	if b, ok := c.blobs[digest]; ok {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	return nil, fmt.Errorf("blob %s not found", digest)
}

func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	p := NewProvider(t.Context())
	p.MacosVNCPassword = "testpass"
	p.Probes = probes.NewManager(t.Context())
	t.Cleanup(p.Close)
	return p
}

func newLeaseParser(t *testing.T, mac, ip string) *network.LeaseParser {
	t.Helper()
	path := filepath.Join(t.TempDir(), "leases.json")
	entry := `[{"mac":"` + mac + `","ip":"` + ip + `","expiry":"2099-01-01T00:00:00Z"}]`
	if err := os.WriteFile(path, []byte(entry), 0o644); err != nil {
		t.Fatalf("write leases: %v", err)
	}
	return network.NewLeaseParser(path)
}

func newPodWithSpec(spec meta.VMSpec) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	spec.Managed = true
	spec.Apply(pod)
	return pod
}

func blobDigest(b []byte) string {
	return "sha256:" + ociutil.SHA256Hex(b)
}

func snapshotArtifact() ([]byte, map[string][]byte) {
	cfg := []byte(`{"schemaVersion":"1","snapshotId":"snap-test"}`)
	m := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"artifactType":%q,`+
		`"config":{"mediaType":%q,"digest":%q,"size":%d},"layers":[]}`,
		manifest.MediaTypeOCIManifest, manifest.ArtifactTypeSnapshot,
		manifest.MediaTypeSnapshotConfig, blobDigest(cfg), len(cfg))
	return []byte(m), map[string][]byte{blobDigest(cfg): cfg}
}

func cloudImageArtifact() ([]byte, map[string][]byte) {
	disk := []byte("qcow2-bytes")
	m := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"artifactType":%q,`+
		`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":%q,"size":2},`+
		`"layers":[{"mediaType":%q,"digest":%q,"size":%d}]}`,
		manifest.MediaTypeOCIManifest, manifest.ArtifactTypeOSImage,
		blobDigest([]byte("{}")), manifest.MediaTypeDiskQcow2, blobDigest(disk), len(disk))
	return []byte(m), map[string][]byte{blobDigest(disk): disk}
}

func newSnapshotTestSetup(t *testing.T) (*Provider, *fakeRuntime, *countingRegistry) {
	t.Helper()
	m, blobs := snapshotArtifact()
	return newArtifactTestSetup(t, m, blobs)
}

func newCloudImageTestSetup(t *testing.T) (*Provider, *fakeRuntime, *countingRegistry) {
	t.Helper()
	m, blobs := cloudImageArtifact()
	return newArtifactTestSetup(t, m, blobs)
}

func newArtifactTestSetup(t *testing.T, m []byte, blobs map[string][]byte) (*Provider, *fakeRuntime, *countingRegistry) {
	t.Helper()
	rt := &fakeRuntime{}
	reg := &countingRegistry{fakeRegistry: fakeRegistry{manifest: m}, blobs: blobs}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Puller = &snapshots.Puller{Registry: reg, Runtime: rt}
	return p, rt, reg
}
