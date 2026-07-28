package cocoon

import (
	"cmp"
	"context"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	resumeOpHibernate   = "hibernate"
	resumeOpPostClone   = "post_clone"
	resumeOpReadyWait   = "ready_wait"
	resumeOpClassifyNIC = "classify_drop_nic"
)

// dispatchOwedWork resumes the step a vk restart interrupted, derived from
// observed state only: the pod's persisted annotations plus the adopted VM.
// Callbacks are one-shot, so nothing else re-delivers this work (#54).
func (p *Provider) dispatchOwedWork() {
	type owed struct {
		pod *corev1.Pod
		v   *vm.VM
		op  string
	}
	p.mu.RLock()
	work := make([]owed, 0, len(p.pods))
	for key, pod := range p.pods {
		if op := owedOpFor(pod, p.vmsByPod[key]); op != "" {
			work = append(work, owed{pod: pod, v: p.vmsByPod[key], op: op})
		}
	}
	p.mu.RUnlock()

	logger := log.WithFunc("Provider.dispatchOwedWork")
	for _, w := range work {
		logger.Warnf(p.lifecycleCtx, "resuming interrupted %s for %s/%s (vm %s)",
			w.op, w.pod.Namespace, w.pod.Name, w.v.ID)
		metrics.StartupResumeTotal.WithLabelValues(w.op).Inc()
		p.emitNormalf(w.pod, "ResumedAfterRestart", "op=%s", w.op)
		p.dispatchResume(w.pod, w.v, w.op)
	}
}

// dispatchResume claims the pod for the whole resumed op — every resume runs
// outside the framework's per-pod serialization, so UpdatePod's acting arms
// must back off from all of them, not just hibernate.
func (p *Provider) dispatchResume(pod *corev1.Pod, v *vm.VM, op string) {
	key := meta.PodKey(pod.Namespace, pod.Name)
	if !p.claimResume(key) {
		return
	}
	run := func(f func()) {
		p.goBackground(func() {
			defer p.releaseResume(key)
			f()
		})
	}
	switch op {
	case resumeOpHibernate:
		run(func() {
			// A VM that crashed while hibernate was owed boots first: nothing
			// else re-delivers the hibernate once supervision restarts it.
			if v.State != vm.StateRunning {
				if err := p.Runtime.Start(p.lifecycleCtx, v.ID); err != nil {
					p.failOp(p.lifecycleCtx, pod, "ResumeStartFailed", "reconcile", err)
					return
				}
			}
			if err := p.hibernate(p.lifecycleCtx, pod, v); err != nil {
				return
			}
			p.refreshStatus(p.lifecycleCtx, pod)
			p.notify(pod)
		})
	case resumeOpPostClone:
		spec := meta.ParseVMSpec(pod)
		run(func() { p.runPostCloneSetup(p.lifecycleCtx, pod, spec, v, "", "reconcile") })
	case resumeOpReadyWait:
		spec := meta.ParseVMSpec(pod)
		run(func() { p.resumeReadyAfterIP(p.lifecycleCtx, pod, spec, v) })
	case resumeOpClassifyNIC:
		spec := meta.ParseVMSpec(pod)
		run(func() {
			// Evidence ⟺ restore, by CreatePod's own fresh-boot guard.
			evidence, ok := p.classifyNICRecovery(pod, spec.VMName)
			switch {
			case !ok:
			case evidence:
				p.resumeReadyAfterIP(p.lifecycleCtx, pod, spec, v)
			default:
				p.runPostCloneSetup(p.lifecycleCtx, pod, spec, v, "", "reconcile")
			}
		})
	}
}

// classifyNICRecovery retries the evidence lookup until the registry answers —
// guessing on an error could mark a fresh clone Ready without its fixup (the
// NopPinger deployment would never surface it). Budget exhaustion fails loud.
func (p *Provider) classifyNICRecovery(pod *corev1.Pod, vmName string) (evidence, ok bool) {
	ctx := p.lifecycleCtx
	delay := cmp.Or(p.deferredRecheckInitialDelay, defaultDeferredRecheckInitialDelay)
	maxDelay := cmp.Or(p.deferredRecheckMaxDelay, defaultDeferredRecheckMaxDelay)
	deadline := time.Now().Add(cmp.Or(p.deferredRecheckBudget, defaultDeferredRecheckBudget))
	for {
		evidence, _, err := p.hibernateEvidence(ctx, vmName)
		if err == nil {
			return evidence, true
		}
		if time.Now().After(deadline) {
			p.failOp(ctx, pod, "ResumeClassifyFailed", "reconcile", err)
			return false, false
		}
		if !commonk8s.SleepCtx(ctx, delay) {
			return false, false
		}
		delay = min(delay*2, maxDelay)
	}
}

// resumeReadyAfterIP re-runs the SAC pass when owed (post-clone-state is
// written before SAC runs, so done does not imply SAC ran), then holds
// Ready until the lease lands.
func (p *Provider) resumeReadyAfterIP(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, v *vm.VM) {
	if p.willRunSAC(spec, v) {
		if _, ok := p.runWindowsSAC(ctx, pod, v, "reconcile"); !ok {
			return
		}
	}
	p.markReadyAfterIP(ctx, pod, v)
}

func (p *Provider) claimResume(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, held := p.resumedOps[key]; held {
		return false
	}
	p.resumedOps[key] = struct{}{}
	return true
}

func (p *Provider) releaseResume(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.resumedOps, key)
}

func (p *Provider) resumeBusy(key string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, held := p.resumedOps[key]
	return held
}

// owedOpFor decides what a tracked pod is still owed. Deleting pods belong to
// DeletePod; a hibernate owed on a crashed VM still dispatches (the resume
// boots it first — nothing else re-delivers the hibernate afterwards).
func owedOpFor(pod *corev1.Pod, v *vm.VM) string {
	if pod.DeletionTimestamp != nil || v == nil {
		return ""
	}
	if meta.ReadHibernateState(pod) {
		return resumeOpHibernate
	}
	lc := meta.ReadLifecycleState(pod)
	pcs := pod.Annotations[annotationPostCloneState]
	// Empty lifecycle with a post-clone marker means the Creating patch was
	// lost to apiserver flakiness; the marker still records owed work.
	if lc != meta.LifecycleStateCreating && (lc != "" || pcs == "") {
		return ""
	}
	switch pcs {
	case postCloneStateFailed:
		return ""
	case postCloneStateRunning:
		return resumeOpPostClone
	case postCloneStateDone:
		return resumeOpReadyWait
	default:
		spec := meta.ParseVMSpec(pod)
		// No marker on a drop-NIC spec is either an interrupted restore (no
		// post-clone, finalize only waits for the lease — PnP must not run
		// over the hot-added NIC) or a fresh clone caught in the ms before
		// its running marker. Only hibernate evidence can tell; resolved
		// asynchronously at dispatch.
		if shouldDropNICBeforeHibernate(spec) {
			return resumeOpClassifyNIC
		}
		if postCloneNeeded(spec, v) {
			return resumeOpPostClone
		}
		return resumeOpReadyWait
	}
}
