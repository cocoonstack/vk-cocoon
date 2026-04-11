package main

import (
	"context"

	corev1 "k8s.io/api/core/v1"
)

// CocoonNodeProvider is the node.NodeProvider implementation for the
// virtual cocoon node. vk-cocoon never pushes asynchronous status
// updates — the virtual-kubelet controller polls via Ping and stamps
// status itself — so the provider is stateless.
type CocoonNodeProvider struct{}

// NewCocoonNodeProvider returns a ready-to-use node provider.
func NewCocoonNodeProvider() *CocoonNodeProvider {
	return &CocoonNodeProvider{}
}

// Ping satisfies node.NodeProvider. The cocoon runtime heartbeat
// lives elsewhere; this method is the node-level keepalive the
// controller uses to keep the node marked Ready.
func (*CocoonNodeProvider) Ping(_ context.Context) error {
	return nil
}

// NotifyNodeStatus is a no-op. vk-cocoon never initiates node status
// pushes; the controller drives refresh via Ping.
func (*CocoonNodeProvider) NotifyNodeStatus(_ context.Context, _ func(*corev1.Node)) {
}
