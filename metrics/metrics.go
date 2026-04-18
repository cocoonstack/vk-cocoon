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

	SnapshotSaveTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Name:      "snapshot_save_total",
			Help:      "Number of snapshot saves by result.",
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

	VMBootDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "vm_boot_duration_seconds",
			Help:      "Time to create a VM (run or clone), from start to Running.",
			Buckets:   []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		},
		[]string{"mode", "backend"}, // mode=run|clone, backend=cloud-hypervisor|firecracker
	)

	SnapshotSaveDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "snapshot_save_duration_seconds",
			Help:      "Time to save a VM snapshot (cocoon snapshot save).",
			Buckets:   []float64{1, 2, 5, 10, 30, 60, 120},
		},
	)

	SnapshotPushDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "snapshot_push_duration_seconds",
			Help:      "Time to push a snapshot to epoch.",
			Buckets:   []float64{1, 5, 10, 30, 60, 120, 300},
		},
	)

	SnapshotPullDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "snapshot_pull_duration_seconds",
			Help:      "Time to pull a snapshot from epoch.",
			Buckets:   []float64{1, 5, 10, 30, 60, 120, 300},
		},
	)

	ProbeDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Name:      "probe_duration_seconds",
			Help:      "Time taken by a single readiness probe (ICMP ping).",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
		},
	)
)

// Register installs all collectors.
func Register(reg prometheus.Registerer) {
	reg.MustRegister(
		PodLifecycleTotal,
		SnapshotSaveTotal,
		SnapshotPullTotal,
		SnapshotPushTotal,
		VMTableSize,
		OrphanVMTotal,
		VMBootDuration,
		SnapshotSaveDuration,
		SnapshotPushDuration,
		SnapshotPullDuration,
		ProbeDuration,
	)
}
