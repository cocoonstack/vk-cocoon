package snapshots

import (
	"context"
	"fmt"
	"io"

	"github.com/cocoonstack/epoch/manifest"
	"github.com/cocoonstack/epoch/snapshot"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// Puller streams a snapshot or cloud image from epoch into the
// local cocoon runtime. It uses epoch/snapshot.Stream and
// epoch/cloudimg.Stream so vk-cocoon never needs to speak OCI
// distribution directly.
type Puller struct {
	Registry RegistryClient
	Runtime  vm.Runtime
}

// PullSnapshot fetches the manifest at (name, tag) and pipes the
// reassembled cocoon-import tar straight into the local
// `cocoon snapshot import --name <localName>` subprocess. localName
// defaults to name when empty.
func (p *Puller) PullSnapshot(ctx context.Context, name, tag, localName string) error {
	raw, _, err := p.Registry.GetManifest(ctx, name, tag)
	if err != nil {
		return fmt.Errorf("get snapshot manifest %s:%s: %w", name, tag, err)
	}
	if localName == "" {
		localName = name
	}

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

// PullCloudImage fetches the manifest at (name, tag) and writes the
// concatenated raw disk bytes to the supplied io.Writer.
func (p *Puller) PullCloudImage(ctx context.Context, name, tag string, w io.Writer) error {
	raw, _, err := p.Registry.GetManifest(ctx, name, tag)
	if err != nil {
		return fmt.Errorf("get cloudimg manifest %s:%s: %w", name, tag, err)
	}
	// Verify it really is a cloud-image manifest before streaming;
	// snapshots and container images would corrupt the destination
	// if treated as raw disk bytes.
	kind, err := manifest.Classify(raw)
	if err != nil {
		return fmt.Errorf("classify manifest: %w", err)
	}
	if kind != manifest.KindCloudImage {
		return fmt.Errorf("manifest %s:%s is not a cloud image (kind=%s)", name, tag, kind)
	}
	adapter := blobReader{client: p.Registry, name: name}
	return cloudimgStream(ctx, raw, adapter, w)
}
