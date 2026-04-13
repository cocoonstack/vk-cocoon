package cocoon

import (
	"context"

	corev1 "k8s.io/api/core/v1"
)

// NodeProvider is the stateless node.NodeProvider for the virtual cocoon node.
type NodeProvider struct{}

// NewNodeProvider returns a ready-to-use node provider.
func NewNodeProvider() *NodeProvider {
	return &NodeProvider{}
}

// Ping satisfies node.NodeProvider.
func (*NodeProvider) Ping(_ context.Context) error {
	return nil
}

// NotifyNodeStatus is a no-op; the controller drives refresh via Ping.
func (*NodeProvider) NotifyNodeStatus(_ context.Context, _ func(*corev1.Node)) {
}
