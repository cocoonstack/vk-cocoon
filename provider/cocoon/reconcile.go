package cocoon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"

	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// StartupReconcile rebuilds the in-memory tables from K8s pods and cocoon VMs; unmatched VMs are handled per OrphanPolicy.
func (p *Provider) StartupReconcile(ctx context.Context) error {
	logger := log.WithFunc("Provider.StartupReconcile")

	var (
		pods *corev1.PodList
		vms  []vm.VM
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		list, err := p.Clientset.CoreV1().Pods(metav1.NamespaceAll).List(gctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + p.NodeName,
		})
		if err != nil {
			return fmt.Errorf("list pods on %s: %w", p.NodeName, err)
		}
		pods = list
		return nil
	})
	g.Go(func() error {
		list, err := p.Runtime.List(gctx)
		if err != nil {
			return fmt.Errorf("list local VMs: %w", err)
		}
		vms = list
		return nil
	})
	if err := g.Wait(); err != nil {
		return err
	}
	for i := range pods.Items {
		if err := p.assertPodSnapshotCompatibility(&pods.Items[i]); err != nil {
			return fmt.Errorf("startup compatibility check: %w", err)
		}
	}
	vms = p.reconcileStaleCreates(ctx, vms)

	vmByID := make(map[string]*vm.VM, len(vms))
	vmByName := make(map[string]*vm.VM, len(vms))
	for i := range vms {
		vmByID[vms[i].ID] = &vms[i]
		if vms[i].Name != "" {
			vmByName[vms[i].Name] = &vms[i]
		}
	}
	matched := make(map[string]bool, len(vms))
	type macosPod struct {
		pod  *corev1.Pod
		spec meta.VMSpec
	}
	var probePods []*corev1.Pod
	var macosPods []macosPod

	for i := range pods.Items {
		pod := &pods.Items[i]
		// os=macos guests live outside Runtime.List; adopt them via cocoon-macos.
		if spec := meta.ParseVMSpec(pod); isMacosSpec(spec) {
			macosPods = append(macosPods, macosPod{pod, spec})
			continue
		}
		runtime := meta.ParseVMRuntime(pod)
		if runtime.VMID == "" {
			if v := p.adoptByVMName(ctx, pod, vmByName); v != nil {
				matched[v.ID] = true
				probePods = append(probePods, pod)
				continue
			}
			p.reconcileNoVMID(ctx, pod)
			continue
		}
		v, ok := vmByID[runtime.VMID]
		if !ok {
			// the VM was removed during hibernate but the annotation patch failed.
			if meta.ReadHibernateState(pod) {
				p.reconcileStaleHibernate(ctx, pod)
				continue
			}
			logger.Warnf(ctx, "pod %s/%s annotates VMID %s but no such VM exists locally; CreatePod will recreate",
				pod.Namespace, pod.Name, runtime.VMID)
			continue
		}
		p.trackPod(pod, v)
		p.seedLifecycleIntentFromPod(pod)
		matched[v.ID] = true
		probePods = append(probePods, pod)
	}
	// first probes run synchronously (3s worst case) and gate node registration; the two fan-outs touch disjoint state, so overlap them too.
	var wg sync.WaitGroup
	wg.Go(func() { fanOut(startupFanOut, probePods, p.startProbeIfEnabled) })
	wg.Go(func() {
		fanOut(startupFanOut, macosPods, func(mp macosPod) {
			p.reconcileMacosPod(ctx, mp.pod, mp.spec)
		})
	})
	wg.Wait()

	for i := range vms {
		if matched[vms[i].ID] {
			continue
		}
		p.handleOrphan(ctx, &vms[i])
	}

	p.dispatchOwedWork()
	logger.Infof(ctx, "startup reconcile: %d pods adopted, %d orphan VMs", len(matched), len(vms)-len(matched))
	return nil
}

// reconcileStaleCreates strips creating placeholders from the startup VM list — adopting one deadlocks its pod (#54).
func (p *Provider) reconcileStaleCreates(ctx context.Context, vms []vm.VM) []vm.VM {
	logger := log.WithFunc("Provider.reconcileStaleCreates")
	keep := make([]*vm.VM, len(vms))
	var stale []int
	for i := range vms {
		if vms[i].State != vm.StateCreating {
			keep[i] = &vms[i]
			continue
		}
		stale = append(stale, i)
	}
	fanOut(startupFanOut, stale, func(i int) {
		v := &vms[i]
		outcome, err := p.Runtime.ReconcileStaleCreate(ctx, v.ID)
		recordStaleCreateOutcome(outcome, err)
		if err != nil {
			logger.Errorf(ctx, err, "reconcile creating placeholder %s (%s); watching for commit", v.ID, v.Name)
			p.watchBusyCreate(v.ID)
			return
		}
		switch outcome {
		case vm.StaleCreateNotCreating:
			if fresh, settled := p.classifySettledCreate(ctx, v.ID); !settled {
				p.watchBusyCreate(v.ID)
			} else if fresh != nil {
				keep[i] = fresh
			}
		case vm.StaleCreateBusy:
			logger.Warnf(ctx, "creating placeholder %s (%s) is owned by an in-flight operation; watching for commit", v.ID, v.Name)
			p.watchBusyCreate(v.ID)
		default:
			logger.Warnf(ctx, "creating placeholder %s (%s): %s", v.ID, v.Name, outcome)
		}
	})
	kept := make([]vm.VM, 0, len(vms))
	for _, v := range keep {
		if v != nil {
			kept = append(kept, *v)
		}
	}
	return kept
}

// watchBusyCreate re-invokes the reclaim verb on an unclassified creating record: only the verb tells a live owner from one that died holding the name.
func (p *Provider) watchBusyCreate(vmID string) {
	p.goBackground(func() {
		ctx := p.lifecycleCtx
		logger := log.WithFunc("Provider.watchBusyCreate")
		delay, maxDelay, budget := p.recheckBackoff()
		deadline := time.Now().Add(budget)
		for {
			if !commonk8s.SleepCtx(ctx, delay) {
				return
			}
			outcome, err := p.Runtime.ReconcileStaleCreate(ctx, vmID)
			recordStaleCreateOutcome(outcome, err)
			if err != nil {
				logger.Errorf(ctx, err, "re-reconcile creating placeholder %s", vmID)
			} else if outcome == vm.StaleCreateCollected || outcome == vm.StaleCreateNotFound {
				logger.Infof(ctx, "in-flight create %s resolved as %s; CreatePod recreates", vmID, outcome)
				return
			}
			// a failing verb must not strand a clone that did commit.
			if fresh, settled := p.classifySettledCreate(ctx, vmID); settled {
				if fresh != nil {
					logger.Infof(ctx, "in-flight create %s (%s) committed; indexing for adoption", vmID, fresh.Name)
					p.indexOrphanByName(fresh)
				}
				return
			}
			if time.Now().After(deadline) {
				logger.Warnf(ctx, "in-flight create %s did not resolve within budget; giving up", vmID)
				return
			}
			delay = min(delay*2, maxDelay)
		}
	})
}

// classifySettledCreate resolves a record past the verb: (vm, true) adoptable, (nil, true) settled, (nil, false) still transitional.
func (p *Provider) classifySettledCreate(ctx context.Context, vmID string) (*vm.VM, bool) {
	logger := log.WithFunc("Provider.classifySettledCreate")
	fresh, err := p.Runtime.Inspect(ctx, vmID)
	switch {
	case errors.Is(err, vm.ErrVMNotFound):
		return nil, true
	case err != nil:
		logger.Errorf(ctx, err, "re-inspect creating placeholder %s; watching for commit", vmID)
		return nil, false
	case fresh.State == vm.StateRunning:
		return fresh, true
	case inFlightCreate(fresh.State):
		return nil, false
	default:
		logger.Warnf(ctx, "placeholder %s (%s) left creating as %s without running; applying orphan policy", vmID, fresh.Name, fresh.State)
		p.handleOrphan(ctx, fresh)
		return nil, true
	}
}

// reconcileStaleHibernate clears stale VMID/IP from a hibernated pod whose VM is already gone, so wake can start clean.
func (p *Provider) reconcileStaleHibernate(ctx context.Context, pod *corev1.Pod) {
	logger := log.WithFunc("Provider.reconcileStaleHibernate")
	logger.Infof(ctx, "pod %s/%s is hibernated with stale VMID, clearing annotations", pod.Namespace, pod.Name)
	if err := p.clearRuntimeAnnotations(ctx, pod); err != nil {
		logger.Errorf(ctx, err, "clear stale hibernate annotations %s/%s", pod.Namespace, pod.Name)
	}
	p.trackPod(pod, nil)
	p.seedLifecycleIntentFromPod(pod)
}

// adoptByVMName re-adopts a live VM by matching pod, the same sequence CreatePod runs on its adopt branch.
func (p *Provider) adoptByVMName(ctx context.Context, pod *corev1.Pod, idx map[string]*vm.VM) *vm.VM {
	logger := log.WithFunc("Provider.adoptByVMName")
	spec := meta.ParseVMSpec(pod)
	if spec.VMName == "" {
		return nil
	}
	v, ok := idx[spec.VMName]
	if !ok {
		return nil
	}
	logger.Infof(ctx, "adopting VM %s by name for pod %s/%s (annotation missing)",
		v.Name, pod.Namespace, pod.Name)
	p.applyRuntime(ctx, pod, v)
	p.trackPod(pod, v)
	p.seedLifecycleIntentFromPod(pod)
	metrics.ReconcileAdoptByNameTotal.Inc()
	return v
}

// reconcileNoVMID handles a pod with no VMID during startup reconcile; hibernated pods are tracked without a VM, others skipped.
func (p *Provider) reconcileNoVMID(ctx context.Context, pod *corev1.Pod) {
	if !meta.ReadHibernateState(pod) {
		return
	}
	p.trackPod(pod, nil)
	p.seedLifecycleIntentFromPod(pod)
	log.WithFunc("Provider.reconcileNoVMID").
		Infof(ctx, "pod %s/%s hibernated, tracking without VM", pod.Namespace, pod.Name)
}

// handleOrphan applies OrphanPolicy to an unmatched VM; non-destroy policies index it by name so a recreated pod adopts it.
func (p *Provider) handleOrphan(ctx context.Context, v *vm.VM) {
	logger := log.WithFunc("Provider.handleOrphan")
	switch p.OrphanPolicy {
	case provider.OrphanDestroy:
		logger.Warnf(ctx, "destroying orphan VM %s (id=%s)", v.Name, v.ID)
		if err := p.removeVM(ctx, v); err != nil {
			logger.Errorf(ctx, err, "remove orphan VM %s", v.ID)
		}
	case provider.OrphanKeep:
		p.indexOrphanByName(v)
	default: // provider.OrphanAlert
		metrics.OrphanVMTotal.Inc()
		logger.Warnf(ctx, "orphan VM detected: name=%s id=%s state=%s ip=%s (set VK_ORPHAN_POLICY=destroy to auto-clean)",
			v.Name, v.ID, v.State, v.IP)
		p.indexOrphanByName(v)
	}
}

// indexOrphanByName exposes an orphan VM to vmByName so the next CreatePod for its pod takes the adopt branch.
func (p *Provider) indexOrphanByName(v *vm.VM) {
	if v.Name == "" {
		return
	}
	p.mu.Lock()
	p.vmsByName[v.Name] = v
	p.mu.Unlock()
}

// inFlightCreate: vm run passes through created between the create-lock windows.
func inFlightCreate(state string) bool {
	return state == vm.StateCreating || state == vm.StateCreated
}

func recordStaleCreateOutcome(outcome vm.StaleCreateOutcome, err error) {
	label := string(outcome)
	if err != nil {
		label = "error"
	}
	metrics.StaleCreateReconcileTotal.WithLabelValues(label).Inc()
}
