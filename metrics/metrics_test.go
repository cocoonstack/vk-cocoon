package metrics

import (
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestWorkloadMetricsExposeNamespace(t *testing.T) {
	const namespace = "testing-cocoonset"

	VMTableSize.WithLabelValues(namespace).Set(1)
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
		VMTableSize,
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
		"cocoon_vk_vm_table_size":                          {"namespace"},
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
