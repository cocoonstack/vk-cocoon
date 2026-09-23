package cocoon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const snapshotCleanupTimeout = 10 * time.Second

// removeSnapshotDetached drops a snapshot under a fresh timed context so caller cancel can't abort it, and a slow remove can't starve a follow-up.
func (p *Provider) removeSnapshotDetached(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
	defer cancel()
	if err := p.Runtime.SnapshotRemoveIfExists(ctx, name); err != nil {
		log.WithFunc("Provider.removeSnapshotDetached").Errorf(ctx, err, "remove snapshot %s", name)
	}
}

// saveAndPushSnapshot saves and pushes a running VM's snapshot; a VM that is not running has no live state to capture and is skipped.
func (p *Provider) saveAndPushSnapshot(ctx context.Context, pod *corev1.Pod, v *vm.VM, tag, image string) error {
	current, err := p.Runtime.Inspect(ctx, v.ID)
	switch {
	case errors.Is(err, vm.ErrVMNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("inspect vm %s: %w", v.ID, err)
	case current.State != vm.StateRunning:
		log.WithFunc("Provider.saveAndPushSnapshot").Warnf(ctx, "vm %s is %s, deleting it without a snapshot", v.ID, current.State)
		return nil
	}

	saveStart := time.Now()
	if err := p.Runtime.SnapshotSave(ctx, v.Name, v.ID); err != nil {
		metrics.SnapshotSaveTotal.WithLabelValues("failed").Inc()
		return fmt.Errorf("save snapshot %s: %w", v.Name, err)
	}
	metrics.SnapshotSaveDuration.WithLabelValues(pod.Namespace).Observe(time.Since(saveStart).Seconds())
	metrics.SnapshotSaveTotal.WithLabelValues("ok").Inc()

	pushStart := time.Now()
	if err := p.Pusher.PushSnapshot(ctx, v.Name, "", tag, image); err != nil {
		metrics.SnapshotPushTotal.WithLabelValues("failed").Inc()
		return fmt.Errorf("push snapshot %s: %w", v.Name, err)
	}
	metrics.SnapshotPushDuration.WithLabelValues(pod.Namespace).Observe(time.Since(pushStart).Seconds())
	metrics.SnapshotPushTotal.WithLabelValues("ok").Inc()
	return nil
}
