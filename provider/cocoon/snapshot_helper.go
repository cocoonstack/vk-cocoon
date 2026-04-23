package cocoon

import (
	"context"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/vk-cocoon/metrics"
)

// saveAndPushSnapshot saves a snapshot and pushes it to epoch, recording
// timing metrics. Errors are logged and counted but not returned — the
// delete path treats snapshot failures as non-fatal.
func (p *Provider) saveAndPushSnapshot(ctx context.Context, vmName, vmID, tag, image string) {
	logger := log.WithFunc("Provider.saveAndPushSnapshot")

	saveStart := time.Now()
	if err := p.Runtime.SnapshotSave(ctx, vmName, vmID); err != nil {
		logger.Warnf(ctx, "snapshot save %s: %v", vmName, err)
		metrics.SnapshotPushTotal.WithLabelValues("failed").Inc()
		return
	}
	metrics.SnapshotSaveDuration.Observe(time.Since(saveStart).Seconds())

	pushStart := time.Now()
	if _, err := p.Pusher.PushSnapshot(ctx, vmName, "", tag, image); err != nil {
		logger.Warnf(ctx, "push snapshot %s: %v", vmName, err)
		metrics.SnapshotPushTotal.WithLabelValues("failed").Inc()
		return
	}
	metrics.SnapshotPushDuration.Observe(time.Since(pushStart).Seconds())
	metrics.SnapshotPushTotal.WithLabelValues("ok").Inc()

	// Mirror base image to epoch for cross-node clone.
	// Get image type and digest from the saved snapshot.
	snap, snapErr := p.Runtime.Snapshot(ctx, vmName)
	if snapErr != nil || snap == nil {
		return
	}
	p.Pusher.MirrorBaseImage(ctx, snap.Image, snap.ImageType, snap.ImageDigest, vmName+"-image")
}
