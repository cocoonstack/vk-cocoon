package cocoon

import (
	"context"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/vk-cocoon/provider"
)

const nodeStorageRefreshInterval = time.Minute

type storageAllocatableFunc func(held int64) (resource.Quantity, bool, error)

// NodeProvider pushes the node's ephemeral-storage allocatable as VM disk usage moves; the node controller drives everything else via Ping.
type NodeProvider struct {
	base     *corev1.Node
	held     func() int64
	storage  storageAllocatableFunc
	lastSent resource.Quantity
}

// NewNodeProvider snapshots base, the node as registered, as the template of every status push; a nil base disables pushes.
func NewNodeProvider(p *Provider, base *corev1.Node) *NodeProvider {
	n := &NodeProvider{held: p.vmDiskHeld, storage: provider.StorageAllocatable}
	if base != nil {
		n.base = base.DeepCopy()
		n.lastSent = n.base.Status.Allocatable[corev1.ResourceEphemeralStorage]
	}
	return n
}

func (*NodeProvider) Ping(_ context.Context) error {
	return nil
}

func (n *NodeProvider) NotifyNodeStatus(ctx context.Context, cb func(*corev1.Node)) {
	if n.base == nil {
		return
	}
	go func() {
		n.pushStorage(ctx, cb)
		commonk8s.RunTicker(ctx, nodeStorageRefreshInterval, func(ctx context.Context) { n.pushStorage(ctx, cb) })
	}()
}

// pushStorage hands the controller a full node, since it replaces status, labels and annotations with what it receives.
func (n *NodeProvider) pushStorage(ctx context.Context, cb func(*corev1.Node)) {
	allocatable, ok, err := n.storage(n.held())
	if err != nil {
		log.WithFunc("cocoon.NodeProvider.pushStorage").Warnf(ctx, "refresh ephemeral-storage allocatable: %v", err)
		return
	}
	if !ok || allocatable.Cmp(n.lastSent) == 0 {
		return
	}
	n.lastSent = allocatable
	node := n.base.DeepCopy()
	node.Status.Allocatable[corev1.ResourceEphemeralStorage] = allocatable
	cb(node)
}
