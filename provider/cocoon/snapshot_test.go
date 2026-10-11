package cocoon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/snapshots"
	"github.com/cocoonstack/vk-cocoon/vm"
)

type manifestRecordingRegistry struct {
	fakeRegistry
	mu   sync.Mutex
	refs []string
}

func (r *manifestRecordingRegistry) PutManifest(_ context.Context, name, tag string, _ []byte, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, name+":"+tag)
	return nil
}

func TestSnapshotRequestPushesARunningVMAndAnswers(t *testing.T) {
	p, pod, rt, reg, v := newSnapshotRequestFixture(t, "user-snapshots/u1/s1:v2")

	if err := p.applyUpdate(t.Context(), pod, meta.ParseVMSpec(pod), v, false, pendingSnapshotRef(pod)); err != nil {
		t.Fatalf("applyUpdate: %v", err)
	}
	if rt.snapshotSaveCount != 1 || rt.savedSnapshot.name != "snap-"+v.Name || rt.savedSnapshot.vmID != v.ID {
		t.Errorf("saves = %d of %q from %q, want one of snap-%s from %s", rt.snapshotSaveCount, rt.savedSnapshot.name, rt.savedSnapshot.vmID, v.Name, v.ID)
	}
	if !slices.Equal(rt.exported, []string{"snap-" + v.Name}) {
		t.Errorf("exported %v, want [snap-%s]", rt.exported, v.Name)
	}
	if !slices.Equal(reg.refs, []string{"user-snapshots/u1/s1:v2"}) {
		t.Errorf("pushed %v, want [user-snapshots/u1/s1:v2]", reg.refs)
	}
	if !slices.Contains(rt.snapshotRemoveCalls, "snap-"+v.Name) {
		t.Errorf("removed %v, want the local snap-%s dropped after the push", rt.snapshotRemoveCalls, v.Name)
	}
	want := meta.SnapshotStatus{Ref: "user-snapshots/u1/s1:v2", State: meta.SnapshotStatePushed}
	if got := liveSnapshotStatus(t, p, pod); got != want {
		t.Errorf("status = %+v, want %+v", got, want)
	}
}

func TestSnapshotRequestAnsweredOnThePodIsNotRepeated(t *testing.T) {
	p, pod, rt, _, v := newSnapshotRequestFixture(t, "user-snapshots/u1/s1")
	for k, val := range (meta.SnapshotStatus{Ref: "user-snapshots/u1/s1", State: meta.SnapshotStatePushed}).Annotations() {
		pod.Annotations[k] = val.(string)
	}

	if err := p.applyUpdate(t.Context(), pod, meta.ParseVMSpec(pod), v, false, pendingSnapshotRef(pod)); err != nil {
		t.Fatalf("applyUpdate: %v", err)
	}
	if rt.snapshotSaveCount != 0 {
		t.Errorf("saves = %d, want none for an answered ref", rt.snapshotSaveCount)
	}
}

func TestSnapshotRequestSaveFailureAnswersFailed(t *testing.T) {
	p, pod, rt, reg, v := newSnapshotRequestFixture(t, "user-snapshots/u1/s1")
	rt.snapshotSaveErr = errors.New("save boom")

	if err := p.applyUpdate(t.Context(), pod, meta.ParseVMSpec(pod), v, false, pendingSnapshotRef(pod)); err != nil {
		t.Fatalf("applyUpdate: %v", err)
	}
	got := liveSnapshotStatus(t, p, pod)
	if got.Ref != "user-snapshots/u1/s1" || got.State != meta.SnapshotStateFailed || !strings.Contains(got.Message, "save boom") {
		t.Errorf("status = %+v, want failed with the save error", got)
	}
	if len(reg.refs) != 0 {
		t.Errorf("pushed %v, want nothing", reg.refs)
	}
}

func TestSnapshotRequestPendingAcrossARestartIsResumed(t *testing.T) {
	p, pod, _, reg, v := newSnapshotRequestFixture(t, "user-snapshots/u1/s1:v2")
	pod.Annotations[meta.AnnotationLifecycleState] = string(meta.LifecycleStateReady)
	p.trackPod(pod, v)

	if op := owedOpFor(pod, v); op != resumeOpSnapshot {
		t.Fatalf("owedOpFor = %q, want %q", op, resumeOpSnapshot)
	}
	p.dispatchOwedWork()
	p.bgWG.Wait()
	if !slices.Equal(reg.refs, []string{"user-snapshots/u1/s1:v2"}) {
		t.Errorf("pushed %v, want [user-snapshots/u1/s1:v2]", reg.refs)
	}
	want := meta.SnapshotStatus{Ref: "user-snapshots/u1/s1:v2", State: meta.SnapshotStatePushed}
	if got := liveSnapshotStatus(t, p, pod); got != want {
		t.Errorf("status = %+v, want %+v", got, want)
	}
}

func newSnapshotRequestFixture(t *testing.T, ref string) (*Provider, *corev1.Pod, *snapshotExportRuntime, *manifestRecordingRegistry, *vm.VM) {
	t.Helper()
	v := &vm.VM{ID: "vmid-snap", Name: "vk-ns-demo-0-505043", State: vm.StateRunning}
	rt := &snapshotExportRuntime{fakeRuntime: &fakeRuntime{inspectVM: v}, export: snapshotExportTar(t, "snap-"+v.Name)}
	reg := &manifestRecordingRegistry{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Pusher = &snapshots.Pusher{Runtime: rt, Registry: reg}
	pod := newPodWithSpec(meta.VMSpec{VMName: v.Name, Mode: "clone"})
	pod.UID = "uid-snap"
	pod.Annotations[meta.AnnotationSnapshotRequest] = ref
	p.Clientset = fake.NewSimpleClientset(pod.DeepCopy())
	p.trackPod(pod, v)
	return p, pod, rt, reg, v
}

func liveSnapshotStatus(t *testing.T, p *Provider, pod *corev1.Pod) meta.SnapshotStatus {
	t.Helper()
	live, err := p.Clientset.CoreV1().Pods(pod.Namespace).Get(t.Context(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	status, ok := meta.ReadSnapshotStatus(live)
	if !ok {
		t.Fatalf("no snapshot status on the pod: %v", live.Annotations)
	}
	return status
}
