package metrics

import (
	"maps"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/cocoonstack/vk-cocoon/provider"
)

func TestWorkloadMetricsExposeNamespace(t *testing.T) {
	const namespace = "testing-cocoonset"

	VMBootDuration.WithLabelValues(namespace, "clone", "cloud-hypervisor").Observe(1)
	SnapshotSaveDuration.WithLabelValues(namespace).Observe(1)
	SnapshotPushDuration.WithLabelValues(namespace).Observe(1)
	HibernateTotal.WithLabelValues(namespace, "snapshot", "ok").Inc()
	WakeIPWaitTotal.WithLabelValues(namespace, "ok").Inc()
	PeerRestoreDuration.WithLabelValues(namespace).Observe(1)
	ProbeDuration.WithLabelValues(namespace).Observe(1)
	SnapshotPullDuration.Observe(1)

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(
		VMBootDuration,
		SnapshotSaveDuration,
		SnapshotPushDuration,
		HibernateTotal,
		WakeIPWaitTotal,
		PeerRestoreDuration,
		ProbeDuration,
		SnapshotPullDuration,
	)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	want := map[string][]string{
		"cocoon_vk_vm_boot_duration_seconds":               {"backend", "mode", "namespace"},
		"cocoon_vk_snapshot_save_duration_seconds":         {"namespace"},
		"cocoon_vk_snapshot_push_duration_seconds":         {"namespace"},
		"cocoon_vk_hibernate_total":                        {"namespace", "phase", "result"},
		"cocoon_vk_wake_ip_wait_total":                     {"namespace", "result"},
		"cocoon_vk_snapshot_peer_restore_duration_seconds": {"namespace"},
		"cocoon_vk_probe_duration_seconds":                 {"namespace"},
		"cocoon_vk_snapshot_pull_duration_seconds":         {},
	}
	for name, wantLabels := range want {
		if got := metricLabelNames(t, families, name); !slices.Equal(got, wantLabels) {
			t.Errorf("%s labels = %v, want %v", name, got, wantLabels)
		}
	}
}

func TestVMCollectorCountsTrackedVMsPerNamespace(t *testing.T) {
	collector := NewVMCollector(func() ([]provider.VMStats, provider.NodeStats) {
		return []provider.VMStats{
			{VMName: "a", PodName: "a", Namespace: "staging", Backend: "cloud-hypervisor"},
			{VMName: "b", PodName: "b", Namespace: "staging", Backend: "cloud-hypervisor"},
			{VMName: "c", PodName: "c", Namespace: "testing", Backend: "firecracker"},
		}, provider.NodeStats{}
	})
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(collector)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	got := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "cocoon_vk_vm_table_size" {
			continue
		}
		for _, m := range family.Metric {
			got[m.Label[0].GetValue()] = m.GetGauge().GetValue()
		}
	}
	want := map[string]float64{"staging": 2, "testing": 1}
	if !maps.Equal(got, want) {
		t.Fatalf("vm_table_size = %v, want %v", got, want)
	}
}

func metricLabelNames(t *testing.T, families []*dto.MetricFamily, name string) []string {
	t.Helper()
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		if len(family.Metric) != 1 {
			t.Fatalf("%s has %d samples, want 1", name, len(family.Metric))
		}
		labels := make([]string, 0, len(family.Metric[0].Label))
		for _, label := range family.Metric[0].Label {
			labels = append(labels, label.GetName())
		}
		slices.Sort(labels)
		return labels
	}
	t.Fatalf("metric family %s not found", name)
	return nil
}
