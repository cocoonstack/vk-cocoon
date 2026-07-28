package cocoon

import (
	"context"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"

	"github.com/cocoonstack/vk-cocoon/metrics"
)

const (
	eventMessageMaxBytes     = 512
	lifecycleMessageMaxBytes = 4096
)

// failOp records a terminal Pod failure; op is one of create, update, delete.
func (p *Provider) failOp(ctx context.Context, pod *corev1.Pod, reason, op string, err error) {
	metrics.PodLifecycleTotal.WithLabelValues(op, "failed", "").Inc()
	msg := err.Error()
	p.emitWarningf(pod, reason, "%s", truncate(op+": "+msg, eventMessageMaxBytes))
	p.markLifecycleState(ctx, pod, meta.LifecycleStateFailed, truncate(msg, lifecycleMessageMaxBytes))
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

// lifecycleAlreadyFailed gates Ready transitions when another path already failed.
func (p *Provider) lifecycleAlreadyFailed(pod *corev1.Pod) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	cur, ok := p.lifecycleIntent[meta.PodKey(pod.Namespace, pod.Name)]
	return ok && cur.State == meta.LifecycleStateFailed
}
