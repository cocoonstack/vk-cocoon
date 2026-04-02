package provider

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newManagedDeleteTestPod(name, vmID, vmName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "testns",
			Name:      name,
			Annotations: map[string]string{
				AnnManaged: "true",
				AnnVMID:    vmID,
				AnnVMName:  vmName,
			},
		},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}

func TestDeletePodSkipsDestroyWhenAnotherActivePodSharesVMID(t *testing.T) {
	oldPod := newManagedDeleteTestPod("app-old", "vm-shared", "vk-testns-app-0", corev1.PodFailed)
	runningPod := newManagedDeleteTestPod("app-new", "vm-shared", "vk-testns-app-0", corev1.PodRunning)

	p := newTestProviderWithClient(oldPod, runningPod)
	oldKey := podKey(oldPod.Namespace, oldPod.Name)
	newKey := podKey(runningPod.Namespace, runningPod.Name)

	p.pods[oldKey] = oldPod.DeepCopy()
	p.vms[oldKey] = &CocoonVM{
		podNamespace: oldPod.Namespace,
		podName:      oldPod.Name,
		vmID:         "vm-shared",
		vmName:       "vk-testns-app-0",
		managed:      true,
		state:        "running",
	}
	p.pods[newKey] = runningPod.DeepCopy()
	p.vms[newKey] = &CocoonVM{
		podNamespace: runningPod.Namespace,
		podName:      runningPod.Name,
		vmID:         "vm-shared",
		vmName:       "vk-testns-app-0",
		managed:      true,
		state:        "running",
	}

	var calls int
	p.cocoonExecFn = func(_ context.Context, _ ...string) (string, error) {
		calls++
		return "", nil
	}

	if err := p.DeletePod(context.Background(), oldPod); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no cocoon exec calls, got %d", calls)
	}
	if _, ok := p.vms[oldKey]; ok {
		t.Fatalf("expected old pod VM record to be removed")
	}
	if _, ok := p.vms[newKey]; !ok {
		t.Fatalf("expected active sibling VM record to remain")
	}
}

func TestDeletePodFallbackSkipsDestroyWhenAnotherActivePodSharesVMID(t *testing.T) {
	oldPod := newManagedDeleteTestPod("app-old", "vm-shared", "vk-testns-app-0", corev1.PodFailed)
	runningPod := newManagedDeleteTestPod("app-new", "vm-shared", "vk-testns-app-0", corev1.PodRunning)

	p := newTestProviderWithClient(oldPod, runningPod)
	var calls int
	p.cocoonExecFn = func(_ context.Context, _ ...string) (string, error) {
		calls++
		return "", nil
	}

	if err := p.DeletePod(context.Background(), oldPod); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no cocoon exec calls, got %d", calls)
	}
}

func TestDeletePodStillDestroysWhenOnlyVMNameMatches(t *testing.T) {
	oldPod := newManagedDeleteTestPod("app-old", "vm-old", "vk-testns-app-0", corev1.PodFailed)
	oldPod.Annotations[AnnSnapshotPolicy] = "never"
	runningPod := newManagedDeleteTestPod("app-new", "vm-new", "vk-testns-app-0", corev1.PodRunning)

	p := newTestProviderWithClient(oldPod, runningPod)
	oldKey := podKey(oldPod.Namespace, oldPod.Name)
	newKey := podKey(runningPod.Namespace, runningPod.Name)

	p.pods[oldKey] = oldPod.DeepCopy()
	p.vms[oldKey] = &CocoonVM{
		podNamespace: oldPod.Namespace,
		podName:      oldPod.Name,
		vmID:         "vm-old",
		vmName:       "vk-testns-app-0",
		managed:      true,
		state:        "running",
	}
	p.pods[newKey] = runningPod.DeepCopy()
	p.vms[newKey] = &CocoonVM{
		podNamespace: runningPod.Namespace,
		podName:      runningPod.Name,
		vmID:         "vm-new",
		vmName:       "vk-testns-app-0",
		managed:      true,
		state:        "running",
	}

	var calls [][]string
	p.cocoonExecFn = func(_ context.Context, args ...string) (string, error) {
		dup := append([]string(nil), args...)
		calls = append(calls, dup)
		return "", nil
	}

	if err := p.DeletePod(context.Background(), oldPod); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if len(calls) == 0 {
		t.Fatalf("expected cocoon exec calls")
	}
	last := calls[len(calls)-1]
	want := []string{"delete", "--force", "vm-old"}
	if len(last) != len(want) {
		t.Fatalf("last cocoon exec = %v, want %v", last, want)
	}
	for i := range want {
		if last[i] != want[i] {
			t.Fatalf("last cocoon exec = %v, want %v", last, want)
		}
	}
}
