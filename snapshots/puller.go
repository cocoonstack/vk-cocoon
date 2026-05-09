// Package snapshots wraps the epoch SDK for pulling and pushing cocoon VM snapshots.
package snapshots

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cocoonstack/epoch/manifest"
	"github.com/cocoonstack/epoch/registryclient"
	"github.com/cocoonstack/epoch/snapshot"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// Puller streams a snapshot or cloud image from epoch into the local cocoon runtime.
type Puller struct {
	Registry *registryclient.Client
	Runtime  vm.Runtime
}

// PullSnapshot fetches and imports a snapshot from epoch. localName defaults to name.
func (p *Puller) PullSnapshot(ctx context.Context, name, tag, localName string) error {
	raw, _, err := p.Registry.GetManifest(ctx, name, tag)
	if err != nil {
		return fmt.Errorf("get snapshot manifest %s:%s: %w", name, tag, err)
	}
	localName = cmp.Or(localName, name)

	importer, wait, err := p.Runtime.SnapshotImport(ctx, vm.ImportOptions{Name: localName})
	if err != nil {
		return fmt.Errorf("open cocoon snapshot import: %w", err)
	}

	if err := snapshot.Stream(ctx, raw, p.Registry, snapshot.StreamOptions{
		Name:   name,
		Writer: importer,
	}); err != nil {
		_ = importer.Close()
		_ = wait()
		return fmt.Errorf("stream snapshot: %w", err)
	}
	if err := importer.Close(); err != nil {
		_ = wait()
		return fmt.Errorf("close importer: %w", err)
	}
	if err := wait(); err != nil {
		return err
	}
	return nil
}

// PullCloudImage fetches a cloud image manifest and writes raw disk bytes to w.
func (p *Puller) PullCloudImage(ctx context.Context, name, tag string, w io.Writer) error {
	raw, err := p.fetchCloudImageManifest(ctx, name, tag)
	if err != nil {
		return err
	}
	adapter := blobReader{client: p.Registry, name: name}
	return cloudimgStream(ctx, raw, adapter, w)
}

// EnsureCloudImage streams a cloud-image artifact from epoch into `cocoon image
// import <localName>`. force=true bypasses the local-cache short-circuit.
func (p *Puller) EnsureCloudImage(ctx context.Context, name, tag, localName string, force bool) error {
	raw, err := p.fetchCloudImageManifest(ctx, name, tag)
	if err != nil {
		return err
	}
	return p.EnsureCloudImageFromRaw(ctx, name, localName, raw, force)
}

// EnsureCloudImageFromRaw streams raw into `cocoon image import <localName>`.
// Caller MUST have classified raw as KindCloudImage; this skips the fetch +
// classify so callers that already verified the manifest don't pay twice.
func (p *Puller) EnsureCloudImageFromRaw(ctx context.Context, name, localName string, raw []byte, force bool) error {
	localName = cmp.Or(localName, name)
	if !force {
		switch _, err := p.Runtime.Image(ctx, localName); {
		case err == nil:
			return nil
		case errors.Is(err, vm.ErrImageNotFound):
			// fall through to re-import
		default:
			return fmt.Errorf("inspect local image %s: %w", localName, err)
		}
	}

	importer, wait, err := p.Runtime.ImageImport(ctx, vm.ImageImportOptions{Name: localName})
	if err != nil {
		return fmt.Errorf("open cocoon image import: %w", err)
	}
	adapter := blobReader{client: p.Registry, name: name}
	if err := cloudimgStream(ctx, raw, adapter, importer); err != nil {
		_ = importer.Close()
		_ = wait()
		return fmt.Errorf("stream cloud image: %w", err)
	}
	if err := importer.Close(); err != nil {
		_ = wait()
		return fmt.Errorf("close importer: %w", err)
	}
	return wait()
}

// fetchCloudImageManifest fetches and verifies the manifest at name:tag is a
// cloud-image artifact, returning its raw bytes for downstream streaming.
func (p *Puller) fetchCloudImageManifest(ctx context.Context, name, tag string) ([]byte, error) {
	raw, _, err := p.Registry.GetManifest(ctx, name, tag)
	if err != nil {
		return nil, fmt.Errorf("get cloudimg manifest %s:%s: %w", name, tag, err)
	}
	kind, err := manifest.Classify(raw)
	if err != nil {
		return nil, fmt.Errorf("classify manifest: %w", err)
	}
	if kind != manifest.KindCloudImage {
		return nil, fmt.Errorf("manifest %s:%s is not a cloud image (kind=%s)", name, tag, kind)
	}
	return raw, nil
}
