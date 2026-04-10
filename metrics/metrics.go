// Package metrics defines the prometheus collectors vk-cocoon
// publishes alongside its kubelet endpoint. Counters track pod
// lifecycle outcomes by phase, gauges track in-memory VM table
// sizes, and the Register helper installs everything against the
// supplied registry.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricNamespace = "vk_cocoon"
)

var (
	// PodLifecycleTotal counts CreatePod / DeletePod / UpdatePod
	// outcomes by phase ("created", "deleted", "updated", "failed").
	PodLifecycleTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "pod_lifecycle_total",
			Help:      "Number of pod lifecycle operations by outcome.",
		},
		[]string{"op", "result"},
	)

	// SnapshotPullTotal counts snapshot pulls from epoch by result.
	SnapshotPullTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "snapshot_pull_total",
			Help:      "Number of snapshot pulls from epoch by result.",
		},
		[]string{"result"},
	)

	// SnapshotPushTotal counts snapshot pushes to epoch by result.
	SnapshotPushTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "snapshot_push_total",
			Help:      "Number of snapshot pushes to epoch by result.",
		},
		[]string{"result"},
	)

	// VMTableSize is a gauge of the current in-memory VM table size.
	VMTableSize = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Name:      "vm_table_size",
			Help:      "Number of VMs vk-cocoon currently tracks.",
		},
	)

	// OrphanVMTotal counts orphan VMs detected at startup
	// reconcile time.
	OrphanVMTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "orphan_vm_total",
			Help:      "Number of orphan VMs detected during startup reconcile.",
		},
	)
)

// Register installs every collector against the supplied registry.
func Register(reg prometheus.Registerer) {
	reg.MustRegister(
		PodLifecycleTotal,
		SnapshotPullTotal,
		SnapshotPushTotal,
		VMTableSize,
		OrphanVMTotal,
	)
}
