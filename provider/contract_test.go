package provider

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cocoonstack/cocoon-common/meta"
)

func TestDeriveStableVMNameMatchesSharedContract(t *testing.T) {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "testns",
			Name:      "demo-abc123",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "Deployment",
				Name: "demo",
			}},
		},
	}
	p := newTestProviderWithClient(rs)
	p.vms["existing"] = &CocoonVM{vmName: meta.VMNameForDeployment("testns", "demo", 0)}

	pod := newTestPod("demo-abc123-xyz", nil)
	pod.OwnerReferences = []metav1.OwnerReference{{
		Kind: "ReplicaSet",
		Name: "demo-abc123",
	}}

	p.mu.Lock()
	got := p.deriveStableVMNameLocked(context.Background(), pod)
	p.mu.Unlock()

	if want := meta.VMNameForDeployment("testns", "demo", 1); got != want {
		t.Fatalf("stable vm name mismatch: got %q want %q", got, want)
	}
}

func TestForkHelpersMatchSharedContract(t *testing.T) {
	vmName := meta.VMNameForDeployment("testns", "demo", 3)
	if got, want := extractSlotFromVMName(vmName), meta.ExtractSlotFromVMName(vmName); got != want {
		t.Fatalf("slot contract mismatch: got %d want %d", got, want)
	}
	if got, want := mainAgentVMName(vmName), meta.MainAgentVMName(vmName); got != want {
		t.Fatalf("main vm contract mismatch: got %q want %q", got, want)
	}
	if !isMainAgent(meta.VMNameForDeployment("testns", "demo", 0)) {
		t.Fatalf("expected slot-0 vm to be main agent")
	}
}

func TestProviderAnnotationsMatchSharedContract(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				AnnVMName: meta.VMNameForPod("testns", "demo"),
			},
		},
	}
	if got := ann(pod, AnnVMName, ""); got != meta.VMNameForPod("testns", "demo") {
		t.Fatalf("annotation contract mismatch: got %q", got)
	}
}
