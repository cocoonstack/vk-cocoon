package cocoon

import (
	"context"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"
)

const (
	lifecyclePatchAttempts     = 3
	lifecyclePatchInterval     = 500 * time.Millisecond
	lifecycleReconcileInterval = 15 * time.Second
)

// StartLifecycleReconciler runs the annotation re-flush loop; NotifyPods only pushes pod.Status.
func (p *Provider) StartLifecycleReconciler() {
	p.goBackground(func() {
		p.runLifecycleReconciler(p.lifecycleCtx)
	})
}

func (p *Provider) markLifecycleState(ctx context.Context, pod *corev1.Pod, state meta.LifecycleState, message string) {
	status, applied := p.setLifecycleState(ctx, pod, state, message)
	if applied {
		p.flushLifecycle(ctx, pod.Namespace, pod.Name, status)
	}
}

func (p *Provider) setLifecycleState(ctx context.Context, pod *corev1.Pod, state meta.LifecycleState, message string) (meta.LifecycleStatus, bool) {
	p.mu.Lock()
	status, applied := p.applyLifecycleLocked(ctx, pod, state, message)
	p.mu.Unlock()
	return status, applied
}

// markReadyPublished stages ready in memory, then status must persist before the annotation is exposed.
func (p *Provider) markReadyPublished(ctx context.Context, pod *corev1.Pod) {
	status, applied := p.setLifecycleState(ctx, pod, meta.LifecycleStateReady, "")
	if !applied {
		p.refreshStatus(ctx, pod)
		_ = p.publishPodStatus(ctx, pod)
		p.notify(pod)
		return
	}
	p.publishReadyLifecycle(ctx, pod, status)
}

func (p *Provider) publishReadyLifecycle(ctx context.Context, pod *corev1.Pod, status meta.LifecycleStatus) {
	p.refreshStatus(ctx, pod)
	if err := p.publishPodStatus(ctx, pod); err != nil {
		return
	}
	p.flushLifecycle(ctx, pod.Namespace, pod.Name, status)
	p.notify(pod)
}

// applyLifecycleLocked requires p.mu held; the caller flushes the returned status outside the lock when applied.
func (p *Provider) applyLifecycleLocked(ctx context.Context, pod *corev1.Pod, state meta.LifecycleState, message string) (meta.LifecycleStatus, bool) {
	key := meta.PodKey(pod.Namespace, pod.Name)
	// Async paths capture an old pod pointer; tracked pod's gen is always fresher.
	gen := meta.ReadCocoonSetGeneration(pod)
	tracked := p.pods[key]
	status := meta.LifecycleStatus{State: state, ObservedGeneration: gen, Message: message}
	if tracked != nil {
		// A delete-then-recreate reuses the key; drop a write from the old incarnation.
		if tracked.UID != pod.UID {
			return status, false
		}
		status.ObservedGeneration = max(gen, meta.ReadCocoonSetGeneration(tracked))
	}
	if cur, ok := p.lifecycleIntent[key]; ok && status.ObservedGeneration < cur.ObservedGeneration {
		log.WithFunc("Provider.applyLifecycleLocked").Infof(ctx,
			"drop stale lifecycle write for %s/%s: %s/gen=%d < intent %s/gen=%d",
			pod.Namespace, pod.Name, status.State, status.ObservedGeneration,
			cur.State, cur.ObservedGeneration)
		return status, false
	}
	// Same-gen Failed is sticky (closes the lifecycleAlreadyFailed TOCTOU); only Creating/Hibernating start a new attempt and may clear it.
	if cur, ok := p.lifecycleIntent[key]; ok &&
		cur.State == meta.LifecycleStateFailed &&
		state != meta.LifecycleStateFailed &&
		state != meta.LifecycleStateCreating &&
		state != meta.LifecycleStateHibernating &&
		status.ObservedGeneration == cur.ObservedGeneration {
		log.WithFunc("Provider.applyLifecycleLocked").Infof(ctx,
			"drop %s/%s %s at gen=%d over sticky Failed", pod.Namespace, pod.Name, state, gen)
		return status, false
	}
	p.lifecycleIntent[key] = status
	status.Apply(pod)
	if tracked != nil && tracked != pod {
		// Keep tracked pod in sync so GetPod's DeepCopy reflects the new state.
		status.Apply(tracked)
	}
	return status, true
}

func (p *Provider) flushLifecycle(ctx context.Context, namespace, name string, status meta.LifecycleStatus) {
	logger := log.WithFunc("Provider.flushLifecycle")
	annos := status.Annotations()
	key := meta.PodKey(namespace, name)
	snap := status.Snapshot()
	var lastErr error
	for range lifecyclePatchAttempts {
		// Skip if intent advanced or pod was forgotten — a newer flush owns the write.
		p.mu.RLock()
		cur, ok := p.lifecycleIntent[key]
		p.mu.RUnlock()
		if !ok || cur.Snapshot() != snap {
			return
		}
		err := p.patchPodAnnotations(ctx, namespace, name, annos)
		if err == nil {
			p.recordLifecycleFlushed(key, snap)
			return
		}
		if apierrors.IsNotFound(err) {
			// Pod deleted on apiserver; drop tracking so reconciler stops retrying.
			p.mu.Lock()
			delete(p.lifecycleIntent, key)
			delete(p.lifecycleFlushed, key)
			p.mu.Unlock()
			return
		}
		lastErr = err
		if !commonk8s.SleepCtx(ctx, lifecyclePatchInterval) {
			return
		}
	}
	logger.Errorf(ctx, lastErr, "lifecycle patch failed for %s/%s after %d attempts, will reconcile",
		namespace, name, lifecyclePatchAttempts)
}

func (p *Provider) runLifecycleReconciler(ctx context.Context) {
	commonk8s.RunTicker(ctx, lifecycleReconcileInterval, p.reconcileAllLifecycle)
}

func (p *Provider) reconcileAllLifecycle(ctx context.Context) {
	type lcDrift struct {
		ns, name string
		status   meta.LifecycleStatus
	}

	p.mu.RLock()
	drifts := make([]lcDrift, 0, len(p.lifecycleIntent))
	for key, intent := range p.lifecycleIntent {
		if p.lifecycleFlushed[key] == intent.Snapshot() {
			continue
		}
		ns, name := splitPodKey(key)
		drifts = append(drifts, lcDrift{ns: ns, name: name, status: intent})
	}
	p.mu.RUnlock()

	for _, d := range drifts {
		if d.status.State == meta.LifecycleStateReady {
			pod, err := p.GetPod(ctx, d.ns, d.name)
			if err != nil {
				continue
			}
			p.publishReadyLifecycle(ctx, pod, d.status)
			continue
		}
		p.flushLifecycle(ctx, d.ns, d.name, d.status)
	}
}

// seedLifecycleIntentFromPod restores intent from the pod's annotations on startup so post-restart gen-stamps still echo.
func (p *Provider) seedLifecycleIntentFromPod(pod *corev1.Pod) {
	if meta.ReadLifecycleState(pod) == "" {
		return
	}
	status := meta.ReadLifecycleStatus(pod)
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	p.lifecycleIntent[key] = status
	p.lifecycleFlushed[key] = status.Snapshot()
	p.mu.Unlock()
}

// republishLifecycleOnGenerationBump re-marks current state on a bare gen-stamp UpdatePod; otherwise observed-generation freezes.
func (p *Provider) republishLifecycleOnGenerationBump(ctx context.Context, pod *corev1.Pod) {
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	cur, ok := p.lifecycleIntent[key]
	if !ok || meta.ReadCocoonSetGeneration(pod) <= cur.ObservedGeneration {
		p.mu.Unlock()
		return
	}
	// Read and apply under one lock: a replay of a stale capture could resurrect a state a concurrent write just superseded.
	status, applied := p.applyLifecycleLocked(ctx, pod, cur.State, cur.Message)
	p.mu.Unlock()
	if applied {
		p.flushLifecycle(ctx, pod.Namespace, pod.Name, status)
	}
}

func (p *Provider) recordLifecycleFlushed(key, snap string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, ok := p.lifecycleIntent[key]
	if !ok || cur.Snapshot() != snap {
		return
	}
	p.lifecycleFlushed[key] = snap
}

func splitPodKey(key string) (string, string) {
	ns, name, _ := strings.Cut(key, "/")
	return ns, name
}
