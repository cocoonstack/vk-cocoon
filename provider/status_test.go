package provider

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFindOtherActivePodForVMIDIgnoresStaleInMemoryEntriesWithoutPod(t *testing.T) {
	p := newTestProvider()
	p.vms["other"] = &CocoonVM{
		vmID:   "vm-123",
		vmName: "vk-testns-other-0",
		state:  stateRunning,
	}

	if got := p.findOtherActivePodForVMID(context.Background(), newTestPod("app", nil), "vm-123"); got != "" {
		t.Fatalf("findOtherActivePodForVMID = %q, want empty for stale in-memory entry", got)
	}
}

func TestFindOtherActivePodForVMIDReturnsLivePod(t *testing.T) {
	p := newTestProvider()
	p.pods = map[string]*corev1.Pod{
		"testns/other": {
			ObjectMeta: metav1.ObjectMeta{Namespace: "testns", Name: "other"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
	}
	p.vms["testns/other"] = &CocoonVM{vmID: "vm-123", vmName: "vk-testns-other-0", state: stateRunning}

	if got := p.findOtherActivePodForVMID(context.Background(), newTestPod("app", nil), "vm-123"); got != "testns/other" {
		t.Fatalf("findOtherActivePodForVMID = %q, want live pod key", got)
	}
}
