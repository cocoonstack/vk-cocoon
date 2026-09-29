package cocoon

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestNodeProviderPushesOnlyAChangedStorageAllocatable(t *testing.T) {
	base := &corev1.Node{Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("4"),
		corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
	}}}
	answers := []resource.Quantity{resource.MustParse("10Gi"), resource.MustParse("12Gi"), resource.MustParse("12Gi")}
	var heldSeen []int64
	n := &NodeProvider{
		base: base,
		held: func() int64 { return 7 },
		storage: func(held int64) (resource.Quantity, bool, error) {
			heldSeen = append(heldSeen, held)
			q := answers[0]
			answers = answers[1:]
			return q, true, nil
		},
		lastSent: resource.MustParse("10Gi"),
	}
	var pushed []*corev1.Node
	for range 3 {
		n.pushStorage(t.Context(), func(node *corev1.Node) { pushed = append(pushed, node) })
	}
	if len(pushed) != 1 {
		t.Fatalf("pushed %d times, want once for the one change", len(pushed))
	}
	got := pushed[0].Status.Allocatable
	if eph := got[corev1.ResourceEphemeralStorage]; eph.Cmp(resource.MustParse("12Gi")) != 0 {
		t.Errorf("pushed ephemeral-storage = %s, want 12Gi", eph.String())
	}
	if cpu := got[corev1.ResourceCPU]; cpu.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("pushed cpu = %s, want the base 4", cpu.String())
	}
	if eph := base.Status.Allocatable[corev1.ResourceEphemeralStorage]; eph.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Errorf("base mutated to %s", eph.String())
	}
	for _, h := range heldSeen {
		if h != 7 {
			t.Errorf("storage saw held=%d, want 7", h)
		}
	}
}

func TestNodeProviderSkipsAPinnedOrFailedRefresh(t *testing.T) {
	for name, storage := range map[string]storageAllocatableFunc{
		"pinned": func(int64) (resource.Quantity, bool, error) { return resource.Quantity{}, false, nil },
		"failed": func(int64) (resource.Quantity, bool, error) { return resource.Quantity{}, false, errors.New("statfs") },
	} {
		t.Run(name, func(t *testing.T) {
			n := &NodeProvider{base: &corev1.Node{}, held: func() int64 { return 0 }, storage: storage}
			n.pushStorage(t.Context(), func(*corev1.Node) { t.Error("pushed a node") })
		})
	}
}
