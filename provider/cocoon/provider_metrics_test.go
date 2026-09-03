package cocoon

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestVMTableSizeTracksEachNamespace(t *testing.T) {
	const (
		stagingNS = "metrics-test-staging"
		testingNS = "metrics-test-testing"
	)
	metrics.VMTableSize.DeleteLabelValues(stagingNS)
	metrics.VMTableSize.DeleteLabelValues(testingNS)
	t.Cleanup(func() {
		metrics.VMTableSize.DeleteLabelValues(stagingNS)
		metrics.VMTableSize.DeleteLabelValues(testingNS)
	})

	p := newTestProvider(t)
	stagingA := newPodWithSpecForNamespace(stagingNS, "agent-a")
	stagingB := newPodWithSpecForNamespace(stagingNS, "agent-b")
	testingA := newPodWithSpecForNamespace(testingNS, "agent-a")

	p.trackPod(stagingA, nil)
	assertVMTableSizeAbsent(t, stagingNS)

	p.trackPod(stagingA, &vm.VM{ID: "staging-a", Name: "staging-a"})
	assertVMTableSize(t, stagingNS, 1)

	p.trackPod(stagingA, &vm.VM{ID: "staging-a-new", Name: "staging-a-new"})
	assertVMTableSize(t, stagingNS, 1)

	p.trackPod(stagingB, &vm.VM{ID: "staging-b", Name: "staging-b"})
	p.trackPod(testingA, &vm.VM{ID: "testing-a", Name: "testing-a"})
	assertVMTableSize(t, stagingNS, 2)
	assertVMTableSize(t, testingNS, 1)

	p.forgetVMOnly(stagingNS, stagingA.Name)
	assertVMTableSize(t, stagingNS, 1)
	p.forgetVMOnly(stagingNS, stagingA.Name)
	assertVMTableSize(t, stagingNS, 1)

	p.forgetPod(stagingNS, stagingB.Name)
	p.forgetPod(testingNS, testingA.Name)
	assertVMTableSizeAbsent(t, stagingNS)
	assertVMTableSizeAbsent(t, testingNS)
}

func newPodWithSpecForNamespace(namespace, name string) *corev1.Pod {
	pod := newPodWithSpec(meta.VMSpec{VMName: name, Mode: "run"})
	pod.Namespace = namespace
	pod.Name = name
	return pod
}

func assertVMTableSize(t *testing.T, namespace string, want float64) {
	t.Helper()
	got, ok := vmTableSize(t, namespace)
	if !ok {
		t.Fatalf("VM table size for %s is absent, want %v", namespace, want)
	}
	if got != want {
		t.Fatalf("VM table size for %s = %v, want %v", namespace, got, want)
	}
}

func assertVMTableSizeAbsent(t *testing.T, namespace string) {
	t.Helper()
	if got, ok := vmTableSize(t, namespace); ok {
		t.Fatalf("VM table size for %s = %v, want absent", namespace, got)
	}
}

func vmTableSize(t *testing.T, namespace string) (float64, bool) {
	t.Helper()
	collected := make(chan prometheus.Metric)
	go func() {
		metrics.VMTableSize.Collect(collected)
		close(collected)
	}()
	var (
		value float64
		found bool
	)
	for sample := range collected {
		metric := &dto.Metric{}
		if err := sample.Write(metric); err != nil {
			t.Fatalf("read VM table size sample: %v", err)
		}
		for _, label := range metric.Label {
			if label.GetName() == "namespace" && label.GetValue() == namespace {
				value = metric.GetGauge().GetValue()
				found = true
			}
		}
	}
	return value, found
}
