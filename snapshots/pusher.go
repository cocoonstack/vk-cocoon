package snapshots

import (
	"context"
	"fmt"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/epoch/snapshot"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// Pusher streams a snapshot from the local cocoon runtime up into
// epoch via epoch/snapshot.Pusher. The vm.Runtime adapter satisfies
// epoch's CocoonRunner interface.
type Pusher struct {
	Registry RegistryClient
	Runtime  vm.Runtime
}

// PushSnapshot snapshots the named VM and uploads it to the
// supplied epoch repo at the given tag. baseImage is optional and
// gets stamped into the manifest annotations for downstream
// fork tracking.
func (p *Pusher) PushSnapshot(ctx context.Context, vmName, repo, tag, baseImage string) (*snapshot.PushResult, error) {
	if repo == "" {
		repo = vmName
	}
	if tag == "" {
		tag = meta.DefaultSnapshotTag
	}

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
