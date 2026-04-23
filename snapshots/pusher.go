package snapshots

import (
	"cmp"
	"context"
	"fmt"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/epoch/registryclient"
	"github.com/cocoonstack/epoch/snapshot"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// Pusher streams a local snapshot up into epoch.
type Pusher struct {
	Registry *registryclient.Client
	Runtime  vm.Runtime
}

// PushSnapshot uploads a snapshot to epoch at the given repo/tag.
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
