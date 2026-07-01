package snapshots

import (
	"cmp"
	"context"
	"fmt"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/oci"
	"github.com/cocoonstack/cocoon-common/snapshot"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// Pusher streams a local snapshot up into an OCI registry.
type Pusher struct {
	Registry oci.Registry
	Runtime  vm.Runtime
}

// PushSnapshot uploads a snapshot to the registry at the given repo/tag.
func (p *Pusher) PushSnapshot(ctx context.Context, vmName, repo, tag, baseImage string) (*snapshot.PushResult, error) {
	repo = cmp.Or(repo, vmName)
	tag = cmp.Or(tag, meta.DefaultSnapshotTag)

	pusher := &snapshot.Pusher{
		Uploader: p.Registry,
		Cocoon:   runnerAdapter{Runtime: p.Runtime},
	}

	res, err := pusher.Push(ctx, snapshot.PushOptions{
		Name:      repo,
		Tag:       tag,
		BaseImage: baseImage,
	})
	if err != nil {
		return nil, fmt.Errorf("push snapshot %s:%s: %w", repo, tag, err)
	}
	return res, nil
}
