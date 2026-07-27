package cocoon

import (
	"context"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/vk-cocoon/metrics"
)

const snapshotCleanupTimeout = 10 * time.Second

// removeSnapshotDetached drops a local snapshot under a fresh timed
// context so caller cancel (vk shutdown, kubelet deadline) can't abort
// the rm mid-flight. Per-call timeout, so a slow first remove can't
// starve a follow-up one in the same cleanup pass.
func (p *Provider) removeSnapshotDetached(ctx context.Context, funcLabel, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
	defer cancel()
	if err := p.Runtime.SnapshotRemoveIfExists(ctx, name); err != nil {
		log.WithFunc(funcLabel).Errorf(ctx, err, "remove snapshot %s", name)
	}
}

// saveAndPushSnapshot saves a snapshot and pushes it to the registry, recording
// timing metrics. Errors are logged and counted but not returned — the
// delete path treats snapshot failures as non-fatal.
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
	if _, err := p.Pusher.PushSnapshot(ctx, vmName, "", tag, image); err != nil {
		logger.Errorf(ctx, err, "push snapshot %s", vmName)
		metrics.SnapshotPushTotal.WithLabelValues("failed").Inc()
		return
	}
	metrics.SnapshotPushDuration.Observe(time.Since(pushStart).Seconds())
	metrics.SnapshotPushTotal.WithLabelValues("ok").Inc()
}
