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

// Provider is the interface that all vk-cocoon provider implementations must satisfy.
// It extends nodeutil.Provider with lifecycle hooks needed by the main binary.
type Provider interface {
	nodeutil.Provider

	// StartupReconcile rebuilds in-memory state from K8s and the local runtime.
	StartupReconcile(ctx context.Context) error
	// StartVMWatcher launches a background goroutine that reacts to VM events.
	StartVMWatcher(ctx context.Context)
	// Close releases resources held by the provider.
	Close()
}
