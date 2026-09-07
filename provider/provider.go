// Package provider holds the virtual-kubelet scaffolding shared across cocoon backends: orphan policy, capacity, stats types.
package provider

import (
	"fmt"
	"strings"
	"time"
)

const (
	OrphanAlert   OrphanPolicy = "alert"
	OrphanDestroy OrphanPolicy = "destroy"
	OrphanKeep    OrphanPolicy = "keep"
)

// OrphanPolicy controls what happens to VMs with no matching pod at startup reconcile.
type OrphanPolicy string

// VMStats holds per-VM resource usage for metrics collection.
type VMStats struct {
	VMName    string
	PodName   string
	Namespace string
	Backend   string
	Tap       string
	StartedAt time.Time

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

// Sample is one scrape's worth of stats: per-VM usage, node usage and the tracked-VM count per namespace.
type Sample struct {
	VMs                   []VMStats
	Node                  NodeStats
	TrackedVMsByNamespace map[string]int
}

// ParseOrphanPolicy validates a configured orphan policy, normalizing case.
func ParseOrphanPolicy(s string) (OrphanPolicy, error) {
	switch p := OrphanPolicy(strings.ToLower(strings.TrimSpace(s))); p {
	case OrphanAlert, OrphanDestroy, OrphanKeep:
		return p, nil
	default:
		return "", fmt.Errorf("orphan policy must be %s, %s or %s, got %q", OrphanAlert, OrphanDestroy, OrphanKeep, s)
	}
}
