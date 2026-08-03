package snapshots

import (
	"cmp"
	"context"
	"fmt"

	"golang.org/x/sync/semaphore"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/oci"
	"github.com/cocoonstack/cocoon-common/snapshot"

	"github.com/cocoonstack/vk-cocoon/vm"
)

// AnnotationFromNode names the node that pushed a hibernate snapshot, so a
// wake elsewhere can fetch the raw files from it.
const AnnotationFromNode = "cocoonstack.snapshot.from-node"

// pushGate serializes v2 (pipelined) pushes node-wide — each buffers up to its
// memory budget; v1 spool pushes cost disk, not RAM, and stay concurrent.
var pushGate = semaphore.NewWeighted(1)

// Pusher streams a local snapshot up into an OCI registry. Non-empty NodeName
// is stamped onto the manifest for peer discovery.
type Pusher struct {
	Registry oci.Registry
	Runtime  vm.Runtime
	Transfer TransferConfig
	NodeName string
}

// PushSnapshot uploads a snapshot to the registry at the given repo/tag.
func (p *Pusher) PushSnapshot(ctx context.Context, vmName, repo, tag, baseImage string) (*snapshot.PushResult, error) {
	repo = cmp.Or(repo, vmName)
	tag = cmp.Or(tag, meta.DefaultSnapshotTag)

	if p.Transfer.ZstdLevel > 0 || p.Transfer.ChunkSizeMiB > 0 {
		if err := pushGate.Acquire(ctx, 1); err != nil {
			return nil, err
		}
		defer pushGate.Release(1)
	}

	pusher := &snapshot.Pusher{
		Uploader: p.Registry,
		Cocoon:   runnerAdapter{Runtime: p.Runtime},
	}

	var annotations map[string]string
	if p.NodeName != "" {
		annotations = map[string]string{AnnotationFromNode: p.NodeName}
	}
	res, err := pusher.Push(ctx, snapshot.PushOptions{
		Name:            repo,
		Tag:             tag,
		BaseImage:       baseImage,
		Annotations:     annotations,
		ZstdLevel:       p.Transfer.ZstdLevel,
		ChunkSizeMiB:    p.Transfer.ChunkSizeMiB,
		Concurrency:     p.Transfer.Concurrency,
		MemoryBudgetMiB: p.Transfer.MemoryBudgetMiB,
	})
	if err != nil {
		return nil, fmt.Errorf("push snapshot %s:%s: %w", repo, tag, err)
	}
	return res, nil
}
