package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/oci"
)

func TestBuildRegistry(t *testing.T) {
	reg, err := buildRegistry(buildOpts{ociRegistry: "example.com/proj/repo"})
	if err != nil {
		t.Fatalf("buildRegistry: %v", err)
	}
	if _, ok := reg.(*oci.OCIRegistry); !ok {
		t.Fatalf("got %T, want *oci.OCIRegistry", reg)
	}

	if _, err := buildRegistry(buildOpts{}); err == nil {
		t.Fatal("buildRegistry with no OCI_REGISTRY: want error, got nil")
	}
}

func TestApplyNodeLabels(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		"existing":                           "keep",
		meta.LabelSnapshotCompatibilityClass: "stale",
	}}}

	applyNodeLabels(node, "purpose-a", "n2-cascade-lake-v1")
	if got := node.Labels[meta.LabelNodePool]; got != "purpose-a" {
		t.Errorf("node pool = %q, want purpose-a", got)
	}
	if got := node.Labels[meta.LabelSnapshotCompatibilityClass]; got != "n2-cascade-lake-v1" {
		t.Errorf("snapshot compatibility class = %q, want n2-cascade-lake-v1", got)
	}
	if got := node.Labels["existing"]; got != "keep" {
		t.Errorf("existing label = %q, want keep", got)
	}

	applyNodeLabels(node, "purpose-b", "")
	if _, ok := node.Labels[meta.LabelSnapshotCompatibilityClass]; ok {
		t.Error("empty configuration must remove a stale snapshot compatibility label")
	}
}

func TestValidateSnapshotCompatibilityClass(t *testing.T) {
	for _, value := range []string{"", "n2", "n2-cascade-lake-v1"} {
		if err := validateSnapshotCompatibilityClass(value); err != nil {
			t.Errorf("validate %q: %v", value, err)
		}
	}
	for _, value := range []string{"bad/value", "-leading-dash"} {
		if err := validateSnapshotCompatibilityClass(value); err == nil {
			t.Errorf("validate %q: want error, got nil", value)
		}
	}
}
