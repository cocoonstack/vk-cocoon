// Package snapshots pulls and pushes cocoon VM snapshots and cloud images
// to an OCI registry.
package snapshots

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"

	"golang.org/x/sync/semaphore"

	"github.com/cocoonstack/cocoon-common/cloudimg"
	"github.com/cocoonstack/cocoon-common/manifest"
	"github.com/cocoonstack/cocoon-common/oci"
	"github.com/cocoonstack/cocoon-common/snapshot"

	"github.com/cocoonstack/vk-cocoon/vm"
)

const defaultPullBudgetMiB = 2048

var pullGate = semaphore.NewWeighted(2)

// Puller streams a snapshot or cloud image from an OCI registry into the local cocoon runtime.
type Puller struct {
	Registry oci.Registry
	Runtime  vm.Runtime
	Transfer TransferConfig
}

// PullSnapshot fetches and imports a snapshot from the registry. localName defaults to name.
func (p *Puller) PullSnapshot(ctx context.Context, name, tag, localName string) error {
	raw, _, err := p.Registry.GetManifest(ctx, name, tag)
	if err != nil {
		return fmt.Errorf("get snapshot manifest %s:%s: %w", name, tag, err)
	}
	localName = cmp.Or(localName, name)

	// Gate before opening the importer so a queued pull doesn't hold a cocoon subprocess.
	if snapshotNeedsPullGate(raw) {
		if err = pullGate.Acquire(ctx, 1); err != nil {
			return err
		}
		defer pullGate.Release(1)
	}

	importer, wait, err := p.Runtime.SnapshotImport(ctx, localName)
	if err != nil {
		return fmt.Errorf("open cocoon snapshot import: %w", err)
	}

	err = snapshot.Stream(ctx, raw, p.Registry, snapshot.StreamOptions{
		Name:            name,
		Writer:          importer,
		Concurrency:     p.Transfer.Concurrency,
		MemoryBudgetMiB: cmp.Or(p.Transfer.PullBudgetMiB, defaultPullBudgetMiB),
	})
	return finishImport(importer, wait, err, "stream snapshot")
}

// EnsureCloudImageFromRaw streams raw into `cocoon image import <localName>`; caller MUST have classified raw as KindCloudImage.
func (p *Puller) EnsureCloudImageFromRaw(ctx context.Context, name, localName string, raw []byte, force bool) error {
	localName = cmp.Or(localName, name)
	if !force {
		switch err := p.Runtime.Image(ctx, localName); {
		case err == nil:
			return nil
		case errors.Is(err, vm.ErrImageNotFound):
		default:
			return fmt.Errorf("inspect local image %s: %w", localName, err)
		}
	}

	importer, wait, err := p.Runtime.ImageImport(ctx, localName)
	if err != nil {
		return fmt.Errorf("open cocoon image import: %w", err)
	}
	adapter := blobReader{registry: p.Registry, name: name}
	return finishImport(importer, wait, cloudimg.Stream(ctx, raw, adapter, importer), "stream cloud image")
}

// finishImport settles an import pipe: close-then-wait always runs; a stream error takes precedence over drain results.
func finishImport(importer io.WriteCloser, wait func() error, streamErr error, streamOp string) error {
	if streamErr != nil {
		_ = importer.Close()
		_ = wait()
		return fmt.Errorf("%s: %w", streamOp, streamErr)
	}
	if err := importer.Close(); err != nil {
		_ = wait()
		return fmt.Errorf("close importer: %w", err)
	}
	return wait()
}

func snapshotNeedsPullGate(raw []byte) bool {
	m, err := manifest.Parse(raw)
	if err != nil {
		return false
	}
	return m.ArtifactType == manifest.ArtifactTypeSnapshotV2 ||
		m.MediaType == manifest.MediaTypeOCIIndex ||
		m.MediaType == manifest.MediaTypeDockerIndex
}
