package cocoon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/snapshot"

	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestStartupDispatchOwedWork(t *testing.T) {
	const (
		vmName = "vk-ns-demo-0"
		vmID   = "resume-vmid"
	)
	running := vm.VM{ID: vmID, Name: vmName, State: vm.StateRunning, IP: "10.0.0.9"}
	stopped := vm.VM{ID: vmID, Name: vmName, State: "stopped"}

	cases := []struct {
		name      string
		spec      meta.VMSpec
		annotate  func(pod *corev1.Pod) // extra annotations beyond spec+VMID
		deleting  bool
		vms       []vm.VM
		snapshots map[string]*vm.Snapshot
		wantExec  bool
		wantLC    meta.LifecycleState
	}{
		{
			// Points 5+6: hibernate interrupted before Remove leaves the VM
			// alive (NIC-less or already saved) — re-enter and converge. The
			// stale post-clone marker must not survive into the next incarnation.
			name: "hibernating with live VM re-enters hibernate",
			spec: meta.VMSpec{VMName: vmName, Mode: "clone"},
			annotate: func(pod *corev1.Pod) {
				meta.HibernateState(true).Apply(pod)
				pod.Annotations[annotationPostCloneState] = postCloneStateDone
			},
			vms:    []vm.VM{running},
			wantLC: meta.LifecycleStateHibernated,
		},
		{
			// A VM that crashed while hibernate was owed boots first, then
			// hibernates — nothing else re-delivers the op after supervision
			// restarts it.
			name: "hibernating with stopped VM starts it then hibernates",
			spec: meta.VMSpec{VMName: vmName, Mode: "clone"},
			annotate: func(pod *corev1.Pod) {
				meta.HibernateState(true).Apply(pod)
			},
			vms:    []vm.VM{stopped},
			wantLC: meta.LifecycleStateHibernated,
		},
		{
			// Point 3: post-clone interrupted mid-run; FC always needs the fixup.
			name: "creating with post-clone running re-dispatches the fixup",
			spec: meta.VMSpec{VMName: vmName, Mode: "clone", Backend: vm.BackendFirecracker},
			annotate: func(pod *corev1.Pod) {
				pod.Annotations[meta.AnnotationLifecycleState] = string(meta.LifecycleStateCreating)
				pod.Annotations[annotationPostCloneState] = postCloneStateRunning
			},
			vms:      []vm.VM{running},
			wantExec: true,
			wantLC:   meta.LifecycleStateReady,
		},
		{
			// Points 3-tail and 10: post-clone done (or wake finalize lost) —
			// only the Ready-after-lease wait is owed.
			name: "creating with post-clone done resumes the ready wait",
			spec: meta.VMSpec{VMName: vmName, Mode: "clone"},
			annotate: func(pod *corev1.Pod) {
				pod.Annotations[meta.AnnotationLifecycleState] = string(meta.LifecycleStateCreating)
				pod.Annotations[annotationPostCloneState] = postCloneStateDone
			},
			vms:    []vm.VM{running},
			wantLC: meta.LifecycleStateReady,
		},
		{
			name: "creating with no post-clone marker derives the plan",
			spec: meta.VMSpec{VMName: vmName, Mode: "clone"},
			annotate: func(pod *corev1.Pod) {
				pod.Annotations[meta.AnnotationLifecycleState] = string(meta.LifecycleStateCreating)
			},
			vms:    []vm.VM{running},
			wantLC: meta.LifecycleStateReady,
		},
		{
			// Drop-NIC spec + no marker + hibernate evidence = interrupted
			// restore: resume the lease wait, never the PnP fixup.
			name: "drop-nic restore without marker resumes ready wait",
			spec: meta.VMSpec{
				VMName:  vmName,
				Mode:    "clone",
				OS:      string(cocoonv1.OSWindows),
				Backend: string(cocoonv1.BackendCloudHypervisor),
			},
			annotate: func(pod *corev1.Pod) {
				pod.Annotations[meta.AnnotationLifecycleState] = string(meta.LifecycleStateCreating)
			},
			snapshots: map[string]*vm.Snapshot{vmName: {Name: vmName}},
			vms:       []vm.VM{running},
			wantLC:    meta.LifecycleStateReady,
		},
		{
			// Same facts without evidence = a fresh clone caught before its
			// running marker: the full fixup (PnP exec) must re-run.
			name: "drop-nic fresh clone without marker re-runs the fixup",
			spec: meta.VMSpec{
				VMName:  vmName,
				Mode:    "clone",
				OS:      string(cocoonv1.OSWindows),
				Backend: string(cocoonv1.BackendCloudHypervisor),
			},
			annotate: func(pod *corev1.Pod) {
				pod.Annotations[meta.AnnotationLifecycleState] = string(meta.LifecycleStateCreating)
			},
			vms:      []vm.VM{running},
			wantExec: true,
			wantLC:   meta.LifecycleStateReady,
		},
		{
			// The Creating patch can be lost to apiserver flakiness while the
			// post-clone marker landed; the marker alone records owed work.
			name: "empty lifecycle with running marker still resumes",
			spec: meta.VMSpec{VMName: vmName, Mode: "clone", Backend: vm.BackendFirecracker},
			annotate: func(pod *corev1.Pod) {
				pod.Annotations[annotationPostCloneState] = postCloneStateRunning
			},
			vms:      []vm.VM{running},
			wantExec: true,
			wantLC:   meta.LifecycleStateReady,
		},
		{
			name: "post-clone failed stays parked for the operator",
			spec: meta.VMSpec{VMName: vmName, Mode: "clone"},
			annotate: func(pod *corev1.Pod) {
				pod.Annotations[meta.AnnotationLifecycleState] = string(meta.LifecycleStateCreating)
				pod.Annotations[annotationPostCloneState] = postCloneStateFailed
			},
			vms:    []vm.VM{running},
			wantLC: meta.LifecycleStateCreating,
		},
		{
			// Point 4 / Fix D: a pod mid-deletion gets no resumed work.
			name: "deleting pod gets no dispatch",
			spec: meta.VMSpec{VMName: vmName, Mode: "clone"},
			annotate: func(pod *corev1.Pod) {
				meta.HibernateState(true).Apply(pod)
			},
			deleting: true,
			vms:      []vm.VM{running},
			wantLC:   "",
		},
		{
			// Point 7: hibernate finished removing the VM; stale-hibernate
			// reconcile owns it, dispatch stays out.
			name: "hibernated without VM gets no dispatch",
			spec: meta.VMSpec{VMName: vmName, Mode: "clone"},
			annotate: func(pod *corev1.Pod) {
				meta.HibernateState(true).Apply(pod)
			},
			vms:    nil,
			wantLC: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := newPodWithSpec(tc.spec)
			pod.Spec.NodeName = "cocoon-pool"
			if len(tc.vms) > 0 {
				meta.VMRuntime{VMID: tc.vms[0].ID, IP: tc.vms[0].IP}.Apply(pod)
			}
			tc.annotate(pod)
			if tc.deleting {
				now := metav1.NewTime(time.Now())
				pod.DeletionTimestamp = &now
			}

			rt := &fakeRuntime{listVMs: tc.vms, snapshots: tc.snapshots}
			p := newTestProvider(t)
			p.NodeName = "cocoon-pool"
			p.Runtime = rt
			p.Clientset = fake.NewSimpleClientset(pod)

			if err := p.StartupReconcile(t.Context()); err != nil {
				t.Fatalf("StartupReconcile: %v", err)
			}
			// Positive rows must await the async dispatch before Close cancels
			// lifecycleCtx; negative rows just drain and assert nothing ran.
			if tc.wantLC != "" && tc.wantLC != meta.LifecycleStateCreating {
				awaitLifecycle(t, p, "ns", "demo-0", tc.wantLC)
			}
			p.Close()

			wantSaves := 0
			if tc.wantLC == meta.LifecycleStateHibernated {
				wantSaves = 1
			}
			if rt.snapshotSaveCount != wantSaves {
				t.Errorf("snapshot saves = %d, want %d", rt.snapshotSaveCount, wantSaves)
			}
			if gotExec := len(rt.execCalls) > 0; gotExec != tc.wantExec {
				t.Errorf("exec ran = %v (calls %d), want %v", gotExec, len(rt.execCalls), tc.wantExec)
			}
			tracked, err := p.GetPod(t.Context(), "ns", "demo-0")
			if tc.wantLC == "" {
				if err == nil && meta.ReadLifecycleState(tracked) == meta.LifecycleStateHibernated {
					t.Error("no dispatch expected, but hibernate completed")
				}
				return
			}
			if err != nil {
				t.Fatalf("tracked pod: %v", err)
			}
			if got := meta.ReadLifecycleState(tracked); got != tc.wantLC {
				t.Errorf("lifecycle = %q, want %q", got, tc.wantLC)
			}
			if tc.wantLC == meta.LifecycleStateHibernated {
				if v, ok := tracked.Annotations[annotationPostCloneState]; ok {
					t.Errorf("post-clone marker must not survive hibernate, got %q", v)
				}
			}
		})
	}
}

func TestStartupDispatchResumesSACWhenDoneMarkerPredatesIt(t *testing.T) {
	// post-clone-state=done is written before SAC runs, so done alone must
	// not skip the SAC pass: a failing dialer proves it was re-attempted.
	const vmName = "vk-ns-demo-0"
	staticVM := vm.VM{
		ID: "resume-vmid", Name: vmName, State: vm.StateRunning, IP: "10.0.0.9",
		NetworkConfigs: []*vm.NetworkConfig{{Network: &vm.NetworkInfo{IP: "10.0.0.9"}}},
	}
	pod := newPodWithSpec(meta.VMSpec{
		VMName:  vmName,
		Mode:    "clone",
		OS:      string(cocoonv1.OSWindows),
		Backend: string(cocoonv1.BackendCloudHypervisor),
	})
	pod.Spec.NodeName = "cocoon-pool"
	meta.VMRuntime{VMID: staticVM.ID, IP: staticVM.IP}.Apply(pod)
	pod.Annotations[meta.AnnotationLifecycleState] = string(meta.LifecycleStateCreating)
	pod.Annotations[annotationPostCloneState] = postCloneStateDone

	rt := &fakeRuntime{listVMs: []vm.VM{staticVM}}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset(pod)
	p.GuestSAC = failingSACDialer{}

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	awaitLifecycle(t, p, "ns", "demo-0", meta.LifecycleStateFailed)
}

func TestStartupDispatchClassifyRetriesRegistryErrors(t *testing.T) {
	// Guessing restore on a registry error could mark a fresh clone Ready
	// without its fixup; the classifier must retry until the registry answers.
	const vmName = "vk-ns-demo-0"
	winVM := vm.VM{ID: "resume-vmid", Name: vmName, State: vm.StateRunning, IP: "10.0.0.9"}
	pod := newPodWithSpec(meta.VMSpec{
		VMName:  vmName,
		Mode:    "clone",
		OS:      string(cocoonv1.OSWindows),
		Backend: string(cocoonv1.BackendCloudHypervisor),
	})
	pod.Spec.NodeName = "cocoon-pool"
	meta.VMRuntime{VMID: winVM.ID, IP: winVM.IP}.Apply(pod)
	pod.Annotations[meta.AnnotationLifecycleState] = string(meta.LifecycleStateCreating)

	rt := &fakeRuntime{listVMs: []vm.VM{winVM}}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset(pod)
	p.Registry = &flakyEvidenceRegistry{fails: 2}
	p.deferredRecheckInitialDelay = time.Millisecond
	p.deferredRecheckMaxDelay = 2 * time.Millisecond

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	awaitLifecycle(t, p, "ns", "demo-0", meta.LifecycleStateReady)
	p.Close()
	if len(rt.execCalls) == 0 {
		t.Error("no evidence after retries means fresh clone: the fixup must re-run")
	}
}

func TestStartupDispatchClassifyFailsLoudOnHangingRegistry(t *testing.T) {
	// A registry that accepts but never answers must not hold the claim and
	// park the pod in Creating forever; the budget deadline fails it loudly.
	const vmName = "vk-ns-demo-0"
	winVM := vm.VM{ID: "resume-vmid", Name: vmName, State: vm.StateRunning, IP: "10.0.0.9"}
	pod := newPodWithSpec(meta.VMSpec{
		VMName:  vmName,
		Mode:    "clone",
		OS:      string(cocoonv1.OSWindows),
		Backend: string(cocoonv1.BackendCloudHypervisor),
	})
	pod.Spec.NodeName = "cocoon-pool"
	meta.VMRuntime{VMID: winVM.ID, IP: winVM.IP}.Apply(pod)
	pod.Annotations[meta.AnnotationLifecycleState] = string(meta.LifecycleStateCreating)

	rt := &fakeRuntime{listVMs: []vm.VM{winVM}}
	p := newTestProvider(t)
	p.NodeName = "cocoon-pool"
	p.Runtime = rt
	p.Clientset = fake.NewSimpleClientset(pod)
	p.Registry = blockingEvidenceRegistry{}
	p.deferredRecheckInitialDelay = time.Millisecond
	p.deferredRecheckMaxDelay = 2 * time.Millisecond
	p.deferredRecheckBudget = 30 * time.Millisecond

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	awaitLifecycle(t, p, "ns", "demo-0", meta.LifecycleStateFailed)
	p.Close()
	if len(rt.execCalls) != 0 {
		t.Errorf("no fixup may run while classification is unresolved, execs = %d", len(rt.execCalls))
	}
}

func TestUpdatePodBacksOffWhileResumeInFlight(t *testing.T) {
	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "clone"})
	meta.HibernateState(true).Apply(pod)

	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.trackPod(pod, &vm.VM{ID: "resume-vmid", Name: "vk-ns-demo-0", State: vm.StateRunning})

	key := meta.PodKey(pod.Namespace, pod.Name)
	if !p.claimResume(key) {
		t.Fatal("claim should succeed")
	}
	err := p.UpdatePod(t.Context(), pod)
	if err == nil || !strings.Contains(err.Error(), "resumed operation") {
		t.Fatalf("err = %v, want resume backoff", err)
	}
	if rt.snapshotSaveCount != 0 {
		t.Errorf("hibernate must not run concurrently with the resume, saves = %d", rt.snapshotSaveCount)
	}
	p.releaseResume(key)
	if err := p.UpdatePod(t.Context(), pod); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if rt.snapshotSaveCount != 1 {
		t.Errorf("hibernate should run after release, saves = %d", rt.snapshotSaveCount)
	}
}

// awaitLifecycle polls until the tracked pod reaches want; the resumed ops run
// on lifecycleCtx, so asserting after Close would race its cancellation.
func awaitLifecycle(t *testing.T, p *Provider, namespace, name string, want meta.LifecycleState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pod, err := p.GetPod(t.Context(), namespace, name)
		if err == nil && meta.ReadLifecycleState(pod) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	pod, err := p.GetPod(t.Context(), namespace, name)
	t.Fatalf("lifecycle never reached %q (pod: %v, err: %v)", want, pod, err)
}

// flakyEvidenceRegistry errors the manifest fetch a set number of times,
// then reports no hibernate tag.
type flakyEvidenceRegistry struct {
	fakeRegistry
	mu    sync.Mutex
	fails int
}

func (r *flakyEvidenceRegistry) GetManifest(context.Context, string, string) ([]byte, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fails > 0 {
		r.fails--
		return nil, "", errors.New("registry down")
	}
	return nil, "", fmt.Errorf("get manifest: %w", snapshot.ErrManifestNotFound)
}

// blockingEvidenceRegistry accepts the lookup and never answers until the
// caller's context dies.
type blockingEvidenceRegistry struct{ fakeRegistry }

func (blockingEvidenceRegistry) GetManifest(ctx context.Context, _, _ string) ([]byte, string, error) {
	<-ctx.Done()
	return nil, "", ctx.Err()
}
