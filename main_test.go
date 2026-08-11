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
	classified := &corev1.Node{}
	applyNodeLabels(classified, "purpose-a", "n2-cascade-lake-v1")
	if got := classified.Labels[meta.LabelNodePool]; got != "purpose-a" {
		t.Errorf("node pool = %q, want purpose-a", got)
	}
	if got := classified.Labels[meta.LabelSnapshotCompatibilityClass]; got != "n2-cascade-lake-v1" {
		t.Errorf("snapshot compatibility class = %q, want n2-cascade-lake-v1", got)
	}

	unclassified := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"existing": "keep"}}}
	applyNodeLabels(unclassified, "purpose-b", "")
	if _, ok := unclassified.Labels[meta.LabelSnapshotCompatibilityClass]; ok {
		t.Error("unclassified node must not advertise a snapshot compatibility class")
	}
	if got := unclassified.Labels["existing"]; got != "keep" {
		t.Errorf("existing label = %q, want keep", got)
	}
}
