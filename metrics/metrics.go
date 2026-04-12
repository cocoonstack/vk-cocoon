// Package metrics defines the prometheus collectors for vk-cocoon.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricNamespace = "vk_cocoon"
)

var (
	PodLifecycleTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "pod_lifecycle_total",
			Help:      "Number of pod lifecycle operations by outcome.",
		},
		[]string{"op", "result"},
	)

	SnapshotPullTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "snapshot_pull_total",
			Help:      "Number of snapshot pulls from epoch by result.",
		},
		[]string{"result"},
	)

	SnapshotPushTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "snapshot_push_total",
			Help:      "Number of snapshot pushes to epoch by result.",
		},
		[]string{"result"},
	)

	VMTableSize = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Name:      "vm_table_size",
			Help:      "Number of VMs vk-cocoon currently tracks.",
		},
	)

	OrphanVMTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "orphan_vm_total",
			Help:      "Number of orphan VMs detected during startup reconcile.",
		},
	)
)

// Register installs all collectors.
func Register(reg prometheus.Registerer) {
	reg.MustRegister(
		PodLifecycleTotal,
		SnapshotPullTotal,
		SnapshotPushTotal,
		VMTableSize,
		OrphanVMTotal,
	)
}
