package cocoon

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cocoonstack/cocoon-common/meta"
)

func TestAssertPodSnapshotCompatibility(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "vm-0"},
		Spec: corev1.PodSpec{NodeSelector: map[string]string{
			meta.LabelSnapshotCompatibilityClass: "n2-cascade-lake-v1",
		}},
	}

	t.Run("matching", func(t *testing.T) {
		p := &Provider{NodeName: "node-a", SnapshotCompatibilityClass: "n2-cascade-lake-v1"}
		if err := p.assertPodSnapshotCompatibility(pod); err != nil {
			t.Fatalf("matching class rejected: %v", err)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		p := &Provider{NodeName: "node-b", SnapshotCompatibilityClass: "n4-emerald-rapids-v1"}
		err := p.assertPodSnapshotCompatibility(pod)
		if err == nil || !strings.Contains(err.Error(), `provides "n4-emerald-rapids-v1"`) {
			t.Fatalf("mismatch error = %v", err)
		}
	})

	t.Run("unclassified node", func(t *testing.T) {
		p := &Provider{NodeName: "node-c"}
		err := p.assertPodSnapshotCompatibility(pod)
		if err == nil || !strings.Contains(err.Error(), "is unclassified") {
			t.Fatalf("unclassified error = %v", err)
		}
	})

	t.Run("legacy pod", func(t *testing.T) {
		p := &Provider{NodeName: "node-d", SnapshotCompatibilityClass: "n4-emerald-rapids-v1"}
		legacy := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "legacy"}}
		if err := p.assertPodSnapshotCompatibility(legacy); err != nil {
			t.Fatalf("legacy pod rejected: %v", err)
		}
	})
}
