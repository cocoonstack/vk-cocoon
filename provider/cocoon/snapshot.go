package cocoon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/ociutil"
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
	return p.saveAndPush(ctx, pod, v.ID, v.Name, "", tag, image)
}

func (p *Provider) snapshot(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, v *vm.VM, ref string) error {
	localName := "snap-" + v.Name
	repo, tag := ociutil.ParseRef(ref)
	status := meta.SnapshotStatus{Ref: ref, State: meta.SnapshotStatePushed}
	if err := p.saveAndPush(ctx, pod, v.ID, localName, repo, tag, spec.Image); err != nil {
		p.emitWarningf(pod, "SnapshotFailed", "snapshot to %s: %v", ref, err)
		status.State, status.Message = meta.SnapshotStateFailed, err.Error()
	}
	p.removeSnapshotDetached(ctx, localName)
	return patchWithRetry(ctx, func() error {
		return p.patchIncarnationAnnotations(ctx, pod.Namespace, pod.Name, pod.UID, status.Annotations())
	})
}

func (p *Provider) saveAndPush(ctx context.Context, pod *corev1.Pod, vmID, localName, repo, tag, image string) error {
	saveStart := time.Now()
	// Detached: a save killed between CH's pause and resume leaves the VM paused.
	if err := p.Runtime.SnapshotSave(context.WithoutCancel(ctx), localName, vmID); err != nil {
		metrics.SnapshotSaveTotal.WithLabelValues("failed").Inc()
		return fmt.Errorf("save snapshot %s: %w", localName, err)
	}
	metrics.SnapshotSaveDuration.WithLabelValues(pod.Namespace).Observe(time.Since(saveStart).Seconds())
	metrics.SnapshotSaveTotal.WithLabelValues("ok").Inc()

	pushStart := time.Now()
	if err := p.Pusher.PushSnapshot(ctx, localName, repo, tag, image, snapshotAnnotations(ctx, pod)); err != nil {
		metrics.SnapshotPushTotal.WithLabelValues("failed").Inc()
		return fmt.Errorf("push snapshot %s: %w", localName, err)
	}
	metrics.SnapshotPushDuration.WithLabelValues(pod.Namespace).Observe(time.Since(pushStart).Seconds())
	metrics.SnapshotPushTotal.WithLabelValues("ok").Inc()
	return nil
}

// snapshotAnnotations returns the pod's caller annotations for a push; a malformed value pushes without them.
func snapshotAnnotations(ctx context.Context, pod *corev1.Pod) map[string]string {
	annotations, err := meta.ReadSnapshotAnnotations(pod)
	if err != nil {
		log.WithFunc("cocoon.snapshotAnnotations").Warnf(ctx, "push snapshot for %s/%s without caller annotations: %v", pod.Namespace, pod.Name, err)
	}
	return annotations
}
