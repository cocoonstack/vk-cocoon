// Package metrics defines the prometheus collectors for vk-cocoon.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

const (
	metricNamespace = "cocoon"
	metricSubsystem = "vk"

	labelNamespace = "namespace"
	labelResult    = "result"
)

var (
	PodLifecycleTotal = counterVec("pod_lifecycle_total",
		"Number of pod lifecycle operations by op, result, and reason.", []string{"op", labelResult, "reason"})

	SnapshotPullTotal = counterVec("snapshot_pull_total",
		"Number of snapshot pulls from the registry by result.", []string{labelResult})

	SnapshotVerifyTotal = counterVec("snapshot_verify_total",
		"Local snapshot verification against the registry tag at wake: result=ok|stale|error.", []string{labelResult})

	SnapshotSaveTotal = counterVec("snapshot_save_total",
		"Number of snapshot saves by result.", []string{labelResult})

	SnapshotPushTotal = counterVec("snapshot_push_total",
		"Number of snapshot pushes to the registry by result.", []string{labelResult})

	CloneFromDirTotal = counterVec("clone_from_dir_total",
		"Number of annotation-driven clone-from-dir attempts by result.", []string{labelResult})

	OrphanVMTotal = counter("orphan_vm_total",
		"Number of orphan VMs detected during startup reconcile.")

	VMInspectTransientFailTotal = counter("vm_inspect_transient_fail_total",
		"Number of inspect failures after a VM event that were treated as inconclusive rather than VMGone.")

	PodEvictFailureTotal = counter("pod_evict_failure_total",
		"Number of pod evictions that failed to delete the K8s pod after retries.")

	ReconcileAdoptByNameTotal = counter("reconcile_adopt_by_name_total",
		"Number of pods re-adopted during startup reconcile by VMName fallback (annotation patch had failed).")

	StaleCreateReconcileTotal = counterVec("stale_create_reconcile_total",
		"Creating placeholders found at startup reconcile, by cocoon verb outcome.", []string{"outcome"})

	StartupResumeTotal = counterVec("startup_resume_total",
		"Interrupted operations re-dispatched by startup reconcile, by op.", []string{"op"})

	HibernateEvidenceTotal = counterVec("hibernate_evidence_total",
		"Fresh-boot requests intercepted by hibernate-snapshot evidence, by verdict.", []string{"verdict"})

	VMBootDuration = histogramVec("vm_boot_duration_seconds",
		"Time to create a VM (run or clone), from start to Running.",
		[]float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300}, []string{labelNamespace, "mode", "backend"})

	SnapshotSaveDuration = histogramVec("snapshot_save_duration_seconds",
		"Time to save a VM snapshot (cocoon snapshot save).",
		[]float64{1, 2, 5, 10, 30, 60, 120}, []string{labelNamespace})

	SnapshotPushDuration = histogramVec("snapshot_push_duration_seconds",
		"Time to push a snapshot to the registry.",
		[]float64{1, 5, 10, 30, 60, 120, 300}, []string{labelNamespace})

	SnapshotPullDuration = histogram("snapshot_pull_duration_seconds",
		"Time to pull a snapshot from the registry.",
		[]float64{1, 5, 10, 30, 60, 120, 300})

	PeerRestoreTotal = counterVec("snapshot_peer_restore_total",
		"Peer snapshot restores by result; a failure falls back to the registry pull.", []string{labelResult})

	// Healthy local-ssd transfers land ~5s; long tail for degraded links.
	PeerRestoreDuration = histogramVec("snapshot_peer_restore_duration_seconds",
		"Time to stage a snapshot's raw files from a peer node.",
		[]float64{1, 2.5, 5, 7.5, 10, 15, 30, 60, 120}, []string{labelNamespace})

	ProbeDuration = histogramVec("probe_duration_seconds",
		"Time taken by a single readiness probe (ICMP or TCP).",
		[]float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5}, []string{labelNamespace})

	HibernateTotal = counterVec("hibernate_total",
		"Number of hibernate stages by result.", []string{labelNamespace, "phase", labelResult})

	LeaseReleaseTotal = counterVec("lease_release_total",
		"Cocoon-net DHCP lease releases after VM destruction, by result.", []string{labelResult})

	WakeTotal = counterVec("wake_total",
		"Number of wake operations by result.", []string{labelResult})

	WakeIPWaitTotal = counterVec("wake_ip_wait_total",
		"Outcomes of the post-clone and wake DHCP lease wait.", []string{labelNamespace, labelResult})

	WakeRenewNudgeTotal = counterVec("wake_renew_nudge_total",
		"ipconfig /renew nudges sent to Windows guests still lease-less mid lease-wait.", []string{labelResult})

	PostCloneTotal = counterVec("postclone_total",
		"Number of post-clone fixups by guest kind and result.", []string{"kind", labelResult})

	PostCloneRetryAttempts = histogramVec("postclone_retry_attempts",
		"Attempts consumed by post-clone vsock exec by result.",
		[]float64{1, 2, 5, 10, 20, 40, 60}, []string{labelResult})
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
		PeerRestoreTotal,
		PeerRestoreDuration,
		ProbeDuration,
		HibernateTotal,
		LeaseReleaseTotal,
		WakeTotal,
		WakeIPWaitTotal,
		WakeRenewNudgeTotal,
		PostCloneTotal,
		PostCloneRetryAttempts,
	)
}

func counter(name, help string) prometheus.Counter {
	return prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: metricNamespace, Subsystem: metricSubsystem, Name: name, Help: help,
	})
}

func counterVec(name, help string, labels []string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: metricNamespace, Subsystem: metricSubsystem, Name: name, Help: help,
	}, labels)
}

func histogram(name, help string, buckets []float64) prometheus.Histogram {
	return prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: metricNamespace, Subsystem: metricSubsystem, Name: name, Help: help, Buckets: buckets,
	})
}

func histogramVec(name, help string, buckets []float64, labels []string) *prometheus.HistogramVec {
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricNamespace, Subsystem: metricSubsystem, Name: name, Help: help, Buckets: buckets,
	}, labels)
}
