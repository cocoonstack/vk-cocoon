package cocoon

import (
	"context"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/vk-cocoon/metrics"
)

const snapshotCleanupTimeout = 10 * time.Second

// removeSnapshotDetached drops a snapshot under a fresh timed context so caller cancel can't abort it, and a slow remove can't starve a follow-up.
func (p *Provider) removeSnapshotDetached(ctx context.Context, funcLabel, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
	defer cancel()
	if err := p.Runtime.SnapshotRemoveIfExists(ctx, name); err != nil {
		log.WithFunc(funcLabel).Errorf(ctx, err, "remove snapshot %s", name)
	}
}

// saveAndPushSnapshot saves and pushes a snapshot; errors are logged and counted but not returned since the delete path treats them as non-fatal.
func (p *Provider) saveAndPushSnapshot(ctx context.Context, vmName, vmID, tag, image string) {
	logger := log.WithFunc("Provider.saveAndPushSnapshot")

	saveStart := time.Now()
	if err := p.Runtime.SnapshotSave(ctx, vmName, vmID); err != nil {
		logger.Errorf(ctx, err, "snapshot save %s", vmName)
		metrics.SnapshotSaveTotal.WithLabelValues("failed").Inc()
		return
	}
	metrics.SnapshotSaveDuration.Observe(time.Since(saveStart).Seconds())
	metrics.SnapshotSaveTotal.WithLabelValues("ok").Inc()

	pushStart := time.Now()
	if err := p.Pusher.PushSnapshot(ctx, vmName, "", tag, image); err != nil {
		logger.Errorf(ctx, err, "push snapshot %s", vmName)
		metrics.SnapshotPushTotal.WithLabelValues("failed").Inc()
		return
	}
	metrics.SnapshotPushDuration.Observe(time.Since(pushStart).Seconds())
	metrics.SnapshotPushTotal.WithLabelValues("ok").Inc()
}
