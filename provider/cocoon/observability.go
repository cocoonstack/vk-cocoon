package cocoon

import (
	"context"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
)

// failOp records a terminal Pod failure: pod_lifecycle_total counter, Warning
// Event, lifecycle=Failed annotation, and log. Op must be create|update|delete.
func (p *Provider) failOp(ctx context.Context, pod *corev1.Pod, reason, op string, err error) {
	metrics.PodLifecycleTotal.WithLabelValues(op, "failed").Inc()
	p.emitWarningf(pod, reason, "%s: %v", op, err)
	p.markLifecycleState(ctx, pod, meta.LifecycleStateFailed, err.Error())
	log.WithFunc("Provider.failOp").Errorf(ctx, err, "%s/%s %s", pod.Namespace, pod.Name, op)
}

func (p *Provider) emitWarningf(pod *corev1.Pod, reason, format string, args ...any) {
	if p.Recorder != nil {
		p.Recorder.Eventf(pod, corev1.EventTypeWarning, reason, format, args...)
	}
}

func (p *Provider) emitNormalf(pod *corev1.Pod, reason, format string, args ...any) {
	if p.Recorder != nil {
		p.Recorder.Eventf(pod, corev1.EventTypeNormal, reason, format, args...)
	}
}

// lifecycleAlreadyFailed reports whether a concurrent goroutine has marked
// the tracked pod as Failed. Used to gate Ready transitions so a successful
// post-clone exec cannot clobber a prior static-IP / SAC failure.
func (p *Provider) lifecycleAlreadyFailed(pod *corev1.Pod) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	cur, ok := p.lifecycleIntent[meta.PodKey(pod.Namespace, pod.Name)]
	return ok && cur.State == meta.LifecycleStateFailed
}
