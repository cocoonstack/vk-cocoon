// Package provider holds the virtual-kubelet scaffolding shared across cocoon backends: orphan policy, capacity, stats types.
package provider

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

	CPUSeconds          float64 // cumulative CPU seconds
	CPUThrottledSeconds float64
	CPUThrottledPeriods int64
	MemoryRSS           int64 // bytes
	DiskCOW             int64 // bytes (COW overlay actual size)
	NetRxBytes          uint64
	NetTxBytes          uint64
}

// NodeStats holds node-level resource usage for metrics collection.
type NodeStats struct {
	CPUSeconds       float64
	MemoryUsedBytes  int64
	StorageAvailable int64
	StorageTotal     int64
}
