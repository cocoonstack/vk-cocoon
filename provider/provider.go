package provider

import (
	"context"

	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
)

// OrphanPolicy controls what happens to VMs with no matching pod at startup reconcile.
type OrphanPolicy string

const (
	OrphanAlert   OrphanPolicy = "alert"
	OrphanDestroy OrphanPolicy = "destroy"
	OrphanKeep    OrphanPolicy = "keep"
)

// VMStats holds per-VM resource usage for metrics collection.
type VMStats struct {
	VMName    string
	PodName   string
	Namespace string
	Backend   string

	CPUSeconds float64 // cumulative CPU seconds
	MemoryRSS  int64   // bytes
	DiskCOW    int64   // bytes (COW overlay actual size)
	NetRxBytes uint64
	NetTxBytes uint64
}

// NodeStats holds node-level resource usage for metrics collection.
type NodeStats struct {
	CPUSeconds       float64
	MemoryUsedBytes  int64
	StorageAvailable int64
	StorageTotal     int64
}

// Provider is the interface that all vk-cocoon provider implementations must satisfy.
// It extends nodeutil.Provider with lifecycle hooks needed by the main binary.
type Provider interface {
	nodeutil.Provider

	// StartupReconcile rebuilds in-memory state from K8s and the local runtime.
	StartupReconcile(ctx context.Context) error
	// StartVMWatcher launches a background goroutine that reacts to VM events.
	StartVMWatcher(ctx context.Context)
	// CollectVMStats returns per-VM and node-level stats for the Prometheus collector.
	CollectVMStats() ([]VMStats, NodeStats)
	// Close releases resources held by the provider.
	Close()
}
