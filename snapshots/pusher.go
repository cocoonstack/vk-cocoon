package snapshots

import (
	"cmp"
	"context"
	"fmt"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/epoch/snapshot"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// Pusher streams a local snapshot up into epoch.
type Pusher struct {
	Registry RegistryClient
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

// MirrorBaseImage mirrors the source VM's base image to epoch for cross-node clone.
// Non-fatal: logs warnings on failure so snapshot push is not blocked.
func (p *Pusher) MirrorBaseImage(ctx context.Context, image, imageType, imageDigest, imageRepo string) {
	logger := log.WithFunc("snapshots.MirrorBaseImage")
	if image == "" || imageType == "" {
		return
	}

	switch imageType {
	case "oci":
		if _, mirrorErr := p.MirrorOCIImage(ctx, image, imageRepo); mirrorErr != nil {
			logger.Warnf(ctx, "mirror OCI image %s: %v", image, mirrorErr)
		}
	case "cloudimg":
		if mirrorErr := p.MirrorCloudimg(ctx, image, imageRepo, imageDigest); mirrorErr != nil {
			logger.Warnf(ctx, "mirror cloudimg %s: %v", image, mirrorErr)
		}
	default:
		logger.Warnf(ctx, "unknown image type %q for %s, skip mirror", imageType, image)
	}
}
