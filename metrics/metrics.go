// Package metrics defines the prometheus collectors for vk-cocoon.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metric names are cocoon_vk_*.
const (
	metricNamespace = "cocoon"
	metricSubsystem = "vk"

	labelResult = "result"
)

var (
	PodLifecycleTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "pod_lifecycle_total",
			Help:      "Number of pod lifecycle operations by op, result, and reason.",
		},
		// result=ok|failed|skipped; reason=adopted|noop|missing_vmname|no_vm
		[]string{"op", labelResult, "reason"},
	)

	SnapshotPullTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "snapshot_pull_total",
			Help:      "Number of snapshot pulls from the registry by result.",
		},
		[]string{labelResult},
	)

	SnapshotVerifyTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "snapshot_verify_total",
			Help:      "Local snapshot verification against the registry tag at wake: result=ok|stale|error.",
		},
		[]string{labelResult},
	)

	SnapshotSaveTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "snapshot_save_total",
			Help:      "Number of snapshot saves by result.",
		},
		[]string{labelResult},
	)

	SnapshotPushTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "snapshot_push_total",
			Help:      "Number of snapshot pushes to the registry by result.",
		},
		[]string{labelResult},
	)

	CloneFromDirTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "clone_from_dir_total",
			Help:      "Number of annotation-driven clone-from-dir attempts by result.",
		},
		[]string{labelResult},
	)

	VMTableSize = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "vm_table_size",
			Help:      "Number of VMs vk-cocoon currently tracks.",
		},
	)

	OrphanVMTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "orphan_vm_total",
			Help:      "Number of orphan VMs detected during startup reconcile.",
		},
	)

	VMInspectTransientFailTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "vm_inspect_transient_fail_total",
			Help:      "Number of inspect failures after a VM event that were treated as inconclusive rather than VMGone.",
		},
	)

	PodEvictFailureTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "pod_evict_failure_total",
			Help:      "Number of pod evictions that failed to delete the K8s pod after retries.",
		},
	)

	ReconcileAdoptByNameTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "reconcile_adopt_by_name_total",
			Help:      "Number of pods re-adopted during startup reconcile by VMName fallback (annotation patch had failed).",
		},
	)

	StaleCreateReconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "stale_create_reconcile_total",
			Help:      "Creating placeholders found at startup reconcile, by cocoon verb outcome.",
		},
		[]string{"outcome"}, // collected|busy|not-creating|not-found|error
	)

	StartupResumeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "startup_resume_total",
			Help:      "Interrupted operations re-dispatched by startup reconcile, by op.",
		},
		[]string{"op"}, // hibernate|post_clone|ready_wait|classify_drop_nic
	)

	HibernateEvidenceTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "hibernate_evidence_total",
			Help:      "Fresh-boot requests intercepted by hibernate-snapshot evidence, by verdict.",
		},
		[]string{"verdict"}, // restored|image_conflict|source_conflict|unavailable
	)

	VMBootDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "vm_boot_duration_seconds",
			Help:      "Time to create a VM (run or clone), from start to Running.",
			Buckets:   []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		},
		[]string{"mode", "backend"}, // mode=run|clone, backend=cloud-hypervisor|firecracker
	)

	SnapshotSaveDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "snapshot_save_duration_seconds",
			Help:      "Time to save a VM snapshot (cocoon snapshot save).",
			Buckets:   []float64{1, 2, 5, 10, 30, 60, 120},
		},
	)

	SnapshotPushDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "snapshot_push_duration_seconds",
			Help:      "Time to push a snapshot to the registry.",
			Buckets:   []float64{1, 5, 10, 30, 60, 120, 300},
		},
	)

	SnapshotPullDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "snapshot_pull_duration_seconds",
			Help:      "Time to pull a snapshot from the registry.",
			Buckets:   []float64{1, 5, 10, 30, 60, 120, 300},
		},
	)

	ProbeDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "probe_duration_seconds",
			Help:      "Time taken by a single readiness probe (ICMP ping).",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5},
		},
	)

	HibernateTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "hibernate_total",
			Help:      "Number of hibernate stages by result.",
		},
		[]string{"phase", labelResult}, // phase=netresize|snapshot|push|remove, result=ok|failed
	)

	WakeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "wake_total",
			Help:      "Number of wake operations by result.",
		},
		[]string{labelResult},
	)

	WakeIPWaitTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "wake_ip_wait_total",
			Help:      "Outcomes of the post-clone and wake DHCP lease wait.",
		},
		[]string{labelResult}, // result=ok|timeout
	)

	WakeRenewNudgeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "wake_renew_nudge_total",
			Help:      "ipconfig /renew nudges sent to Windows guests still lease-less mid lease-wait.",
		},
		[]string{labelResult}, // result=ok|failed
	)

	PostCloneTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "postclone_total",
			Help:      "Number of post-clone fixups by guest kind and result.",
		},
		[]string{"kind", labelResult}, // kind=windows|linux_static|linux_fc|sac, result=ok|failed
	)

	PostCloneRetryAttempts = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "postclone_retry_attempts",
			Help:      "Attempts consumed by post-clone vsock exec by result.",
			Buckets:   []float64{1, 2, 5, 10, 20, 40, 60},
		},
		[]string{labelResult}, // result=ok|failed
	)
)

// Register installs all collectors.
func Register(reg prometheus.Registerer) {
	reg.MustRegister(
		PodLifecycleTotal,
		SnapshotSaveTotal,
		SnapshotPullTotal,
		SnapshotPushTotal,
		SnapshotVerifyTotal,
		CloneFromDirTotal,
		VMTableSize,
		OrphanVMTotal,
		VMInspectTransientFailTotal,
		PodEvictFailureTotal,
		ReconcileAdoptByNameTotal,
		StaleCreateReconcileTotal,
		StartupResumeTotal,
		HibernateEvidenceTotal,
		VMBootDuration,
		SnapshotSaveDuration,
		SnapshotPushDuration,
		SnapshotPullDuration,
		ProbeDuration,
		HibernateTotal,
		WakeTotal,
		WakeIPWaitTotal,
		WakeRenewNudgeTotal,
		PostCloneTotal,
		PostCloneRetryAttempts,
	)
}
