package main

import (
	"context"

	corev1 "k8s.io/api/core/v1"
)

// CocoonNodeProvider is the stateless node.NodeProvider for the virtual cocoon node.
type CocoonNodeProvider struct{}

// NewCocoonNodeProvider returns a ready-to-use node provider.
func NewCocoonNodeProvider() *CocoonNodeProvider {
	return &CocoonNodeProvider{}
}

// Ping satisfies node.NodeProvider.
func (*CocoonNodeProvider) Ping(_ context.Context) error {
	return nil
}

// NotifyNodeStatus is a no-op; the controller drives refresh via Ping.
func (*CocoonNodeProvider) NotifyNodeStatus(_ context.Context, _ func(*corev1.Node)) {
}
