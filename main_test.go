package main

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/workqueue"

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

	unclassified := &corev1.Node{Labels: map[string]string{"existing": "keep"}}
	applyNodeLabels(unclassified, "purpose-b", "")
	if _, ok := unclassified.Labels[meta.LabelSnapshotCompatibilityClass]; ok {
		t.Error("unclassified node must not advertise a snapshot compatibility class")
	}
	if got := unclassified.Labels["existing"]; got != "keep" {
		t.Errorf("existing label = %q, want keep", got)
	}

	applyNodeLabels(classified, "purpose-a", "")
	if _, ok := classified.Labels[meta.LabelSnapshotCompatibilityClass]; ok {
		t.Error("declassifying must remove the stale snapshot compatibility label")
	}
}

func TestPodQueuesFollowTheClientBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			qps       float32
			burst     int
			undelayed int
			limited   bool
			pastMax   time.Duration
		}{
			{"configured", 100, 400, 400, true, 10 * time.Millisecond},
			{"client-go defaults", 0, 0, rest.DefaultBurst, true, time.Second / time.Duration(rest.DefaultQPS)},
			{"unlimited", -1, 0, 1000, false, podRetryBaseDelay},
		} {
			for queue, l := range podQueues(t, tc.qps, tc.burst) {
				for i := range tc.undelayed {
					if d := l.When(i); d > podRetryBaseDelay {
						t.Fatalf("%s: %s queue delayed pod %d by %v inside the client's burst", tc.name, queue, i, d)
					}
				}
				if d := l.When(tc.undelayed); d > tc.pastMax || tc.limited && d <= podRetryBaseDelay {
					t.Fatalf("%s: %s queue delayed pod %d past the client's burst by %v, want at most %v", tc.name, queue, tc.undelayed, d, tc.pastMax)
				}
			}
		}
	})
}

func TestPodQueuesBackOffAFailingPod(t *testing.T) {
	for _, qps := range []float32{100, -1} {
		for queue, l := range podQueues(t, qps, 400) {
			l.When("pod")
			if d := l.When("pod"); d != 2*podRetryBaseDelay {
				t.Fatalf("qps %v: %s queue retried a failing pod after %v, want %v", qps, queue, d, 2*podRetryBaseDelay)
			}
		}
	}
}

func podQueues(t *testing.T, qps float32, burst int) map[string]workqueue.TypedRateLimiter[any] {
	t.Helper()
	var cfg nodeutil.NodeConfig
	if err := withPodQueueLimits(qps, burst)(&cfg); err != nil {
		t.Fatalf("node option: %v", err)
	}
	var c node.PodControllerConfig
	for _, override := range cfg.PodControllerConfigOpts {
		if err := override(&c); err != nil {
			t.Fatalf("pod controller override: %v", err)
		}
	}
	queues := map[string]workqueue.TypedRateLimiter[any]{
		"sync":   c.SyncPodsFromKubernetesRateLimiter,
		"delete": c.DeletePodsFromKubernetesRateLimiter,
		"status": c.SyncPodStatusFromProviderRateLimiter,
	}
	for name, l := range queues {
		if l == nil {
			t.Fatalf("%s queue keeps virtual-kubelet's default limiter", name)
		}
	}
	return queues
}
