package cocoon

import (
	"context"
	"fmt"

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

// dispatchOwedWork resumes the step a vk restart interrupted (#54): callbacks are one-shot, so nothing else re-delivers this work.
func (p *Provider) dispatchOwedWork() {
	type owed struct {
		key string
		pod *corev1.Pod
		v   *vm.VM
		op  string
	}
	p.mu.RLock()
	work := make([]owed, 0, len(p.pods))
	for key, pod := range p.pods {
		if op := owedOpFor(pod, p.vmsByPod[key]); op != "" {
			work = append(work, owed{key: key, pod: pod, v: p.vmsByPod[key], op: op})
		}
	}
	p.mu.RUnlock()

	logger := log.WithFunc("Provider.dispatchOwedWork")
	for _, w := range work {
		logger.Warnf(p.lifecycleCtx, "resuming interrupted %s for %s/%s (vm %s)",
			w.op, w.pod.Namespace, w.pod.Name, w.v.ID)
		metrics.StartupResumeTotal.WithLabelValues(w.op).Inc()
		p.emitNormalf(w.pod, "ResumedAfterRestart", "op=%s", w.op)
		p.dispatchResume(w.key, w.pod, w.v, w.op)
	}
}

// dispatchResume claims the pod for the whole resumed op: resumes run outside the framework's per-pod serialization; UpdatePod backs off.
func (p *Provider) dispatchResume(key string, pod *corev1.Pod, v *vm.VM, op string) {
	if !p.claimResume(key) {
		return
	}
	run := func(f func()) {
		p.goBackground(func() {
			defer p.releaseResume(key)
			f()
		})
	}
	spec := meta.ParseVMSpec(pod)
	switch op {
	case resumeOpHibernate:
		run(func() {
			// boot unconditionally: the record still reads running after a SIGKILLed VMM and Start no-ops on a live VM; nothing else re-delivers the hibernate.
			if err := p.Runtime.Start(p.lifecycleCtx, v.ID); err != nil {
				p.failOp(p.lifecycleCtx, pod, "ResumeStartFailed", "reconcile", err)
				return
			}
			if err := p.hibernate(p.lifecycleCtx, pod, spec, v); err != nil {
				return
			}
			p.refreshAndNotify(p.lifecycleCtx, pod)
		})
	case resumeOpPostClone:
		run(func() { p.runPostCloneSetup(p.lifecycleCtx, pod, spec, v, "", "reconcile", false) })
	case resumeOpReadyWait:
		// ambiguous create-tail vs wake-finalize: resumed outcomes skip the wake accounting rather than guess.
		run(func() { p.resumeReadyAfterIP(p.lifecycleCtx, pod, spec, v, false) })
	case resumeOpClassifyNIC:
		run(func() {
			// evidence ⟺ restore: CreatePod's fresh-boot guard and conflict gates already ran before this VM could exist (v != nil here).
			evidence, ok := p.classifyNICRecovery(pod, spec.VMName)
			switch {
			case !ok:
			case evidence:
				p.resumeReadyAfterIP(p.lifecycleCtx, pod, spec, v, true)
			default:
				p.runPostCloneSetup(p.lifecycleCtx, pod, spec, v, "", "reconcile", false)
			}
		})
	}
}

// classifyNICRecovery retries the evidence lookup until the registry answers: guessing could mark a fresh clone Ready without its fixup.
func (p *Provider) classifyNICRecovery(pod *corev1.Pod, vmName string) (evidence, ok bool) {
	delay, maxDelay, budget := p.recheckBackoff()
	ctx, cancel := context.WithTimeout(p.lifecycleCtx, budget)
	defer cancel()
	for {
		evidence, _, err := p.hibernateEvidence(ctx, vmName)
		if err == nil {
			return evidence, true
		}
		if p.lifecycleCtx.Err() == nil && commonk8s.SleepCtx(ctx, delay) {
			delay = min(delay*2, maxDelay)
			continue
		}
		if p.lifecycleCtx.Err() == nil {
			p.failOp(p.lifecycleCtx, pod, "ResumeClassifyFailed", "reconcile", err)
		}
		return false, false
	}
}

// resumeReadyAfterIP re-runs the SAC pass when owed (done is written before SAC runs), then holds Ready until the lease lands.
func (p *Provider) resumeReadyAfterIP(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, v *vm.VM, wake bool) {
	if p.willRunSAC(spec, v) {
		if _, ok := p.runWindowsSAC(ctx, pod, v, "reconcile"); !ok {
			return
		}
	}
	p.markReadyAfterIP(ctx, pod, spec, v, wake)
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

// backoffIfResuming rejects a pod-mutating callback while a resumed op holds the claim; claims only shrink after startup, so check-then-act is safe.
func (p *Provider) backoffIfResuming(namespace, name string) error {
	key := meta.PodKey(namespace, name)
	p.mu.RLock()
	_, held := p.resumedOps[key]
	p.mu.RUnlock()
	if held {
		return fmt.Errorf("resumed operation still in flight for %s", key)
	}
	return nil
}

// owedOpFor decides what a tracked pod is still owed.
func owedOpFor(pod *corev1.Pod, v *vm.VM) string {
	if pod.DeletionTimestamp != nil || v == nil {
		return ""
	}
	// macOS guests have no CH resume steps; reconcileMacosPod already re-armed their readiness probe.
	if isMacosVM(v) {
		return ""
	}
	if meta.ReadHibernateState(pod) {
		return resumeOpHibernate
	}
	lc := meta.ReadLifecycleState(pod)
	pcs := pod.Annotations[annotationPostCloneState]
	// an empty lifecycle (lost Creating patch) with a marker still records owed work.
	resuming := lc == meta.LifecycleStateCreating || lc == ""
	if !resuming {
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
		// marker-less drop-NIC = interrupted restore (PnP must not touch the hot-added NIC) or a pre-marker fresh clone; evidence decides, async.
		if shouldDropNICBeforeHibernate(spec) {
			return resumeOpClassifyNIC
		}
		if postCloneNeeded(spec, v) {
			return resumeOpPostClone
		}
		return resumeOpReadyWait
	}
}
