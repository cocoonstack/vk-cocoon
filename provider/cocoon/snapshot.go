package cocoon

import (
	"context"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
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
func (p *Provider) saveAndPushSnapshot(ctx context.Context, pod *corev1.Pod, v *vm.VM, tag, image string) {
	logger := log.WithFunc("Provider.saveAndPushSnapshot")

	saveStart := time.Now()
	if err := p.Runtime.SnapshotSave(ctx, v.Name, v.ID); err != nil {
		logger.Errorf(ctx, err, "snapshot save %s", v.Name)
		metrics.SnapshotSaveTotal.WithLabelValues("failed").Inc()
		return
	}
	metrics.SnapshotSaveDuration.WithLabelValues(pod.Namespace).Observe(time.Since(saveStart).Seconds())
	metrics.SnapshotSaveTotal.WithLabelValues("ok").Inc()

	pushStart := time.Now()
	if err := p.Pusher.PushSnapshot(ctx, v.Name, "", tag, image); err != nil {
		logger.Errorf(ctx, err, "push snapshot %s", v.Name)
		metrics.SnapshotPushTotal.WithLabelValues("failed").Inc()
		return
	}
	metrics.SnapshotPushDuration.WithLabelValues(pod.Namespace).Observe(time.Since(pushStart).Seconds())
	metrics.SnapshotPushTotal.WithLabelValues("ok").Inc()
}
