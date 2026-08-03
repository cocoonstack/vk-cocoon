package snapshots

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/semaphore"

	"github.com/cocoonstack/cocoon-common/manifest"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/oci"
	"github.com/cocoonstack/cocoon-common/snapshot"

	"github.com/cocoonstack/vk-cocoon/vm"
)

// AnnotationFromNode names the node that pushed a hibernate snapshot, so a
// wake elsewhere can fetch the raw files from it. Stamped via a post-push
// manifest amend to leave cocoon-common's PushOptions untouched.
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

	res, err := pusher.Push(ctx, snapshot.PushOptions{
		Name:            repo,
		Tag:             tag,
		BaseImage:       baseImage,
		ZstdLevel:       p.Transfer.ZstdLevel,
		ChunkSizeMiB:    p.Transfer.ChunkSizeMiB,
		Concurrency:     p.Transfer.Concurrency,
		MemoryBudgetMiB: p.Transfer.MemoryBudgetMiB,
	})
	if err != nil {
		return nil, fmt.Errorf("push snapshot %s:%s: %w", repo, tag, err)
	}
	if p.NodeName != "" {
		// Best-effort: an unstamped push just wakes via the registry pull.
		if amendErr := p.amendFromNode(ctx, repo, tag); amendErr != nil {
			log.WithFunc("Pusher.PushSnapshot").Warnf(ctx, "stamp from-node on %s:%s: %v", repo, tag, amendErr)
		}
	}
	return res, nil
}

// amendFromNode re-puts the manifest with the from-node annotation, edited as
// a raw map so unknown fields survive.
func (p *Pusher) amendFromNode(ctx context.Context, repo, tag string) error {
	raw, mediaType, err := p.Registry.GetManifest(ctx, repo, tag)
	if err != nil {
		return err
	}

	var m map[string]any
	if unmarshalErr := json.Unmarshal(raw, &m); unmarshalErr != nil {
		return unmarshalErr
	}

	annotations, _ := m["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
	}

	annotations[AnnotationFromNode] = p.NodeName
	m["annotations"] = annotations
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return p.Registry.PutManifest(ctx, repo, tag, data, cmp.Or(mediaType, manifest.MediaTypeOCIManifest))
}
