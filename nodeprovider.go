package main

import (
	"context"
	"sync"

	corev1 "k8s.io/api/core/v1"
)

// CocoonNodeProvider is the node.NodeProvider implementation that
// drives the virtual node's liveness (Ping) and status updates
// (NotifyNodeStatus). The controller calls Ping on a heartbeat
// interval; any time the in-memory node object mutates we push a
// fresh copy through the notifier.
type CocoonNodeProvider struct {
	mu       sync.Mutex
	node     *corev1.Node
	notifier func(*corev1.Node)
}

// NewCocoonNodeProvider wraps the supplied node object so
// subsequent Update / NotifyNodeStatus calls can fan updates out to
// the virtual-kubelet controller.
func NewCocoonNodeProvider(node *corev1.Node) *CocoonNodeProvider {
	return &CocoonNodeProvider{node: node}
}

// Ping satisfies node.NodeProvider. The cocoon runtime heartbeat
// lives elsewhere; this method is the node-level keepalive the
// controller uses to keep the node marked Ready.
func (p *CocoonNodeProvider) Ping(_ context.Context) error {
	return nil
}

// NotifyNodeStatus registers a callback the controller uses to
// receive fresh node status pushes. Subsequent Update calls fan
// out through this callback.
func (p *CocoonNodeProvider) NotifyNodeStatus(_ context.Context, cb func(*corev1.Node)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notifier = cb
}

// Update swaps the in-memory node object and, when a notifier is
// registered, pushes a deep-copy through the callback. vk-cocoon
// calls this from the DaemonEndpoint patch loop and any future
// capacity refresh.
func (p *CocoonNodeProvider) Update(node *corev1.Node) {
	p.mu.Lock()
	p.node = node
	cb := p.notifier
	p.mu.Unlock()
	if cb != nil && node != nil {
		cb(node.DeepCopy())
	}
}
