// Package provider — NodeProvider implementation for dynamic node conditions.
//
// Reports real host conditions every 30s:
//   - Ready: always true (process is running)
//   - MemoryPressure: MemAvailable < 100Mi
//   - DiskPressure: /data01 available < 15%
//   - PIDPressure: false (not implemented, VMs don't share host PID space)
package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CocoonNodeProvider implements node.NodeProvider with dynamic conditions.
type CocoonNodeProvider struct {
	node *corev1.Node
}

// NewCocoonNodeProvider creates a node provider with a reference to the node.
func NewCocoonNodeProvider(node *corev1.Node) *CocoonNodeProvider {
	return &CocoonNodeProvider{node: node}
}

// Ping checks if the provider is healthy.
func (n *CocoonNodeProvider) Ping(_ context.Context) error {
	return nil // always healthy if process is running
}

// NotifyNodeStatus registers a callback for node status updates.
// Starts a background goroutine that checks host conditions every 30s.
func (n *CocoonNodeProvider) NotifyNodeStatus(ctx context.Context, cb func(*corev1.Node)) {
	go func() {
		// Initial update
		n.updateConditions()
		n.node.Status.Capacity = NodeCapacity()
		n.node.Status.Allocatable = n.node.Status.Capacity
		cb(n.node)

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n.updateConditions()
				n.node.Status.Capacity = NodeCapacity()
				n.node.Status.Allocatable = n.node.Status.Capacity
				cb(n.node)
			}
		}
	}()
	log.WithFunc("provider.NotifyNodeStatus").Info(ctx, "dynamic conditions enabled (30s interval)")
}

// updateConditions computes real node conditions from host metrics.
func (n *CocoonNodeProvider) updateConditions() {
	now := metav1.Now()

	// Memory pressure: MemAvailable < 100Mi
	memInfo := readMeminfo("MemAvailable")
	memAvail := memInfo["MemAvailable"]
	memPressure := corev1.ConditionFalse
	if memAvail > 0 && memAvail < 100*1024*1024 {
		memPressure = corev1.ConditionTrue
	}

	// Disk pressure: /data01 available < 15%
	diskTotal, diskAvail := readHostDiskBytes("/data01")
	diskPressure := corev1.ConditionFalse
	if diskTotal > 0 && diskAvail < diskTotal/7 { // ~15%
		diskPressure = corev1.ConditionTrue
	}

	n.node.Status.Conditions = []corev1.NodeCondition{
		{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			LastHeartbeatTime:  now,
			LastTransitionTime: now,
			Reason:             "KubeletReady",
			Message:            "cocoon provider is ready",
		},
		{
			Type:               corev1.NodeMemoryPressure,
			Status:             memPressure,
			LastHeartbeatTime:  now,
			LastTransitionTime: now,
			Reason:             "MemoryAvailable",
			Message:            fmt.Sprintf("MemAvailable: %d MiB", memAvail/(1024*1024)),
		},
		{
			Type:               corev1.NodeDiskPressure,
			Status:             diskPressure,
			LastHeartbeatTime:  now,
			LastTransitionTime: now,
			Reason:             "DiskAvailable",
			Message:            fmt.Sprintf("/data01: %d GiB free / %d GiB total", diskAvail/(1024*1024*1024), diskTotal/(1024*1024*1024)),
		},
		{
			Type:               corev1.NodePIDPressure,
			Status:             corev1.ConditionFalse,
			LastHeartbeatTime:  now,
			LastTransitionTime: now,
			Reason:             "PIDsAvailable",
			Message:            "VMs use isolated PID namespaces",
		},
	}
}
