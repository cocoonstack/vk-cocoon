package cocoon

import (
	"context"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"
)

const (
	lifecyclePatchAttempts     = 3
	lifecyclePatchInterval     = 500 * time.Millisecond
	lifecycleReconcileInterval = 15 * time.Second
)

type lifecycleEntry struct {
	uid     types.UID
	status  meta.LifecycleStatus
	flushed string
}

// StartLifecycleReconciler runs the annotation re-flush loop; NotifyPods only pushes pod.Status.
func (p *Provider) StartLifecycleReconciler() {
	p.goBackground(func() {
		p.runLifecycleReconciler(p.lifecycleCtx)
	})
}

func (p *Provider) markLifecycleState(ctx context.Context, pod *corev1.Pod, state meta.LifecycleState, message string) {
	status, applied := p.setLifecycleState(ctx, pod, state, message)
	if applied {
		p.flushLifecycle(ctx, pod.Namespace, pod.Name, pod.UID, status)
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
		p.refreshAndNotify(ctx, pod)
		return
	}
	p.publishReadyLifecycle(ctx, pod, status)
}

func (p *Provider) publishReadyLifecycle(ctx context.Context, pod *corev1.Pod, status meta.LifecycleStatus) {
	p.refreshStatus(ctx, pod)
	if err := p.publishPodStatus(ctx, pod); err != nil {
		return
	}
	p.flushLifecycle(ctx, pod.Namespace, pod.Name, pod.UID, status)
	p.notify(pod)
}

// applyLifecycleLocked requires p.mu held; the caller flushes the returned status outside the lock when applied.
func (p *Provider) applyLifecycleLocked(ctx context.Context, pod *corev1.Pod, state meta.LifecycleState, message string) (meta.LifecycleStatus, bool) {
	logger := log.WithFunc("Provider.applyLifecycleLocked")
	key := meta.PodKey(pod.Namespace, pod.Name)
	// async paths capture an old pod pointer; tracked pod's gen is always fresher.
	gen := meta.ReadCocoonSetGeneration(pod)
	tracked := p.pods[key]
	status := meta.LifecycleStatus{State: state, ObservedGeneration: gen, Message: message}
	if tracked != nil {
		// a delete-then-recreate reuses the key; drop a write from the old incarnation.
		if tracked.UID != pod.UID {
			return status, false
		}
		status.ObservedGeneration = max(gen, meta.ReadCocoonSetGeneration(tracked))
	}
	cur, hasIntent := p.lifecycleIntent[key]
	sameIncarnation := hasIntent && cur.uid == pod.UID
	if hasIntent && !sameIncarnation && tracked == nil {
		logger.Infof(ctx,
			"drop %s/%s %s from uid %s: untracked lifecycle intent belongs to uid %s",
			pod.Namespace, pod.Name, state, pod.UID, cur.uid)
		return status, false
	}
	if sameIncarnation && status.ObservedGeneration < cur.status.ObservedGeneration {
		logger.Infof(ctx,
			"drop stale lifecycle write for %s/%s: %s/gen=%d < intent %s/gen=%d",
			pod.Namespace, pod.Name, status.State, status.ObservedGeneration,
			cur.status.State, cur.status.ObservedGeneration)
		return status, false
	}
	// same-gen Failed is sticky (closes the lifecycleAlreadyFailed TOCTOU); only Creating/Hibernating start a new attempt and may clear it.
	if sameIncarnation &&
		cur.status.State == meta.LifecycleStateFailed &&
		state != meta.LifecycleStateFailed &&
		state != meta.LifecycleStateCreating &&
		state != meta.LifecycleStateHibernating &&
		status.ObservedGeneration == cur.status.ObservedGeneration {
		logger.Infof(ctx,
			"drop %s/%s %s at gen=%d over sticky Failed", pod.Namespace, pod.Name, state, gen)
		return status, false
	}
	p.lifecycleIntent[key] = lifecycleEntry{uid: pod.UID, status: status}
	status.Apply(pod)
	if tracked != nil && tracked != pod {
		// keep tracked pod in sync so GetPod's DeepCopy reflects the new state.
		status.Apply(tracked)
	}
	return status, true
}

func (p *Provider) flushLifecycle(ctx context.Context, namespace, name string, uid types.UID, status meta.LifecycleStatus) {
	logger := log.WithFunc("Provider.flushLifecycle")
	annos := status.Annotations()
	key := meta.PodKey(namespace, name)
	snap := status.Snapshot()
	var lastErr error
	for attempt := range lifecyclePatchAttempts {
		// skip if intent advanced, another incarnation staged one, or the pod was forgotten — a newer flush owns the write.
		p.mu.RLock()
		_, owned := p.lifecycleOwnedLocked(key, uid, snap)
		p.mu.RUnlock()
		if !owned {
			return
		}
		err := p.patchIncarnationAnnotations(ctx, namespace, name, uid, annos)
		if err == nil {
			p.recordLifecycleFlushed(key, uid, snap)
			return
		}
		if patchSuperseded(err) {
			p.dropSupersededLifecycle(key, uid, snap)
			return
		}
		lastErr = err
		if attempt == lifecyclePatchAttempts-1 {
			break
		}
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
		uid      types.UID
		status   meta.LifecycleStatus
	}

	p.mu.RLock()
	drifts := make([]lcDrift, 0, len(p.lifecycleIntent))
	for key, intent := range p.lifecycleIntent {
		if intent.flushed == intent.status.Snapshot() {
			continue
		}
		ns, name := splitPodKey(key)
		drifts = append(drifts, lcDrift{ns: ns, name: name, uid: intent.uid, status: intent.status})
	}
	p.mu.RUnlock()

	fanOut(statusReconcileFanOut, drifts, func(d lcDrift) {
		if d.status.State != meta.LifecycleStateReady {
			p.flushLifecycle(ctx, d.ns, d.name, d.uid, d.status)
			return
		}
		if pod, err := p.GetPod(ctx, d.ns, d.name); err == nil {
			p.publishReadyLifecycle(ctx, pod, d.status)
		} else {
			p.flushLifecycle(ctx, d.ns, d.name, d.uid, d.status)
		}
	})
}

// seedLifecycleIntentFromPod restores intent from the pod's annotations on startup so post-restart gen-stamps still echo.
func (p *Provider) seedLifecycleIntentFromPod(pod *corev1.Pod) {
	if meta.ReadLifecycleState(pod) == "" {
		return
	}
	status := meta.ReadLifecycleStatus(pod)
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	p.lifecycleIntent[key] = lifecycleEntry{uid: pod.UID, status: status, flushed: status.Snapshot()}
	p.mu.Unlock()
}

// republishLifecycleOnGenerationBump re-marks current state on a bare gen-stamp UpdatePod; otherwise observed-generation freezes.
func (p *Provider) republishLifecycleOnGenerationBump(ctx context.Context, pod *corev1.Pod) {
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	cur, ok := p.lifecycleIntent[key]
	if !ok || cur.uid != pod.UID || meta.ReadCocoonSetGeneration(pod) <= cur.status.ObservedGeneration {
		p.mu.Unlock()
		return
	}
	// read and apply under one lock: a replay of a stale capture could resurrect a state a concurrent write just superseded.
	status, applied := p.applyLifecycleLocked(ctx, pod, cur.status.State, cur.status.Message)
	p.mu.Unlock()
	if applied {
		p.flushLifecycle(ctx, pod.Namespace, pod.Name, pod.UID, status)
	}
}

func (p *Provider) recordLifecycleFlushed(key string, uid types.UID, snap string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, owned := p.lifecycleOwnedLocked(key, uid, snap)
	if !owned {
		return
	}
	cur.flushed = snap
	p.lifecycleIntent[key] = cur
}

func (p *Provider) dropSupersededLifecycle(key string, uid types.UID, snap string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, owned := p.lifecycleOwnedLocked(key, uid, snap); !owned {
		return
	}
	delete(p.lifecycleIntent, key)
}

// reassertLifecycleLocked re-stamps this incarnation's intent: the framework's snapshot can carry a stale state it would sync back to the apiserver.
func (p *Provider) reassertLifecycleLocked(key string, pod *corev1.Pod) {
	cur, ok := p.lifecycleIntent[key]
	if !ok {
		return
	}
	if cur.uid != pod.UID {
		delete(p.lifecycleIntent, key)
		return
	}
	cur.status.Apply(pod)
}

func (p *Provider) lifecycleOwnedLocked(key string, uid types.UID, snap string) (lifecycleEntry, bool) {
	cur, ok := p.lifecycleIntent[key]
	return cur, ok && cur.uid == uid && cur.status.Snapshot() == snap
}

func splitPodKey(key string) (string, string) {
	ns, name, _ := strings.Cut(key, "/")
	return ns, name
}
