package cocoon

import (
	"context"
	"fmt"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const staleCreateConcurrency = 8

// StartupReconcile rebuilds the in-memory tables from K8s pods and
// cocoon VMs so restarts don't leak VMs or lose pod associations.
// Unmatched VMs are handled per OrphanPolicy.
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
		if err != nil && !apierrors.IsNotFound(err) {
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

	for i := range podItems(pods) {
		pod := &pods.Items[i]
		runtime := meta.ParseVMRuntime(pod)
		if runtime.VMID == "" {
			if v := p.adoptByVMName(ctx, pod, vmByName); v != nil {
				matched[v.ID] = true
				continue
			}
			p.reconcileNoVMID(ctx, pod)
			continue
		}
		v, ok := vmByID[runtime.VMID]
		if !ok {
			// The VM was removed during hibernate but the annotation patch failed.
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
		p.startProbeIfEnabled(pod)
	}

	for i := range vms {
		if matched[vms[i].ID] {
			continue
		}
		p.handleOrphan(ctx, &vms[i])
	}

	logger.Infof(ctx, "startup reconcile: %d pods adopted, %d orphan VMs", len(matched), len(vms)-len(matched))
	return nil
}

// reconcileStaleCreates strips creating placeholders from the startup VM list —
// adopting one deadlocks its pod. cocoon's lock-checked verb decides skeleton
// vs in-flight clone; busy and errors leave the record alone, only unindexed.
// Bounded fan-out: this gates node registration and the records are independent.
func (p *Provider) reconcileStaleCreates(ctx context.Context, vms []vm.VM) []vm.VM {
	logger := log.WithFunc("Provider.reconcileStaleCreates")
	keep := make([]*vm.VM, len(vms))
	var g errgroup.Group
	g.SetLimit(staleCreateConcurrency)
	for i := range vms {
		v := &vms[i]
		if v.State != vm.StateCreating {
			keep[i] = v
			continue
		}
		g.Go(func() error {
			outcome, err := p.Runtime.ReconcileStaleCreate(ctx, v.ID)
			if err != nil {
				metrics.StaleCreateReconcileTotal.WithLabelValues("error").Inc()
				logger.Errorf(ctx, err, "reconcile creating placeholder %s (%s); skipping adoption", v.ID, v.Name)
				return nil
			}
			metrics.StaleCreateReconcileTotal.WithLabelValues(string(outcome)).Inc()
			if outcome == vm.StaleCreateNotCreating {
				// The create committed between List and the verb; adopt the live record.
				fresh, inspectErr := p.Runtime.Inspect(ctx, v.ID)
				if inspectErr != nil {
					logger.Errorf(ctx, inspectErr, "re-inspect %s after not-creating; skipping adoption", v.ID)
					return nil
				}
				keep[i] = fresh
				return nil
			}
			logger.Warnf(ctx, "creating placeholder %s (%s): %s", v.ID, v.Name, outcome)
			return nil
		})
	}
	_ = g.Wait() // workers report per-record outcomes via keep, never errors
	kept := make([]vm.VM, 0, len(vms))
	for _, v := range keep {
		if v != nil {
			kept = append(kept, *v)
		}
	}
	return kept
}

// reconcileStaleHibernate clears stale VMID/IP from a hibernated pod whose
// VM is already gone, so wake can start clean.
func (p *Provider) reconcileStaleHibernate(ctx context.Context, pod *corev1.Pod) {
	logger := log.WithFunc("Provider.reconcileStaleHibernate")
	logger.Infof(ctx, "pod %s/%s is hibernated with stale VMID, clearing annotations", pod.Namespace, pod.Name)
	if err := p.clearRuntimeAnnotations(ctx, pod); err != nil {
		logger.Errorf(ctx, err, "clear stale hibernate annotations %s/%s", pod.Namespace, pod.Name)
	}
	p.trackPod(pod, nil)
	p.seedLifecycleIntentFromPod(pod)
}

// adoptByVMName re-adopts a live VM whose matching pod has no VMID
// annotation, re-runs the runtime-annotation write, and starts probes —
// the same sequence CreatePod runs on its adopt branch.
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
	p.startProbeIfEnabled(pod)
	metrics.ReconcileAdoptByNameTotal.Inc()
	return v
}

// reconcileNoVMID handles a pod with no VMID during startup reconcile.
// Hibernated pods are tracked without a VM; others are skipped.
func (p *Provider) reconcileNoVMID(ctx context.Context, pod *corev1.Pod) {
	if !meta.ReadHibernateState(pod) {
		return
	}
	p.trackPod(pod, nil)
	p.seedLifecycleIntentFromPod(pod)
	log.WithFunc("Provider.reconcileNoVMID").
		Infof(ctx, "pod %s/%s hibernated, tracking without VM", pod.Namespace, pod.Name)
}

// handleOrphan applies OrphanPolicy to an unmatched VM. Non-destroy
// policies index the VM by name so a recreated pod adopts it instead
// of cloning into a name collision.
func (p *Provider) handleOrphan(ctx context.Context, v *vm.VM) {
	logger := log.WithFunc("Provider.handleOrphan")
	switch p.OrphanPolicy {
	case provider.OrphanDestroy:
		logger.Warnf(ctx, "destroying orphan VM %s (id=%s)", v.Name, v.ID)
		if err := p.Runtime.Remove(ctx, v.ID); err != nil {
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

// indexOrphanByName exposes an orphan VM to vmByName so the next
// CreatePod for its pod takes the adopt branch.
func (p *Provider) indexOrphanByName(v *vm.VM) {
	if v.Name == "" {
		return
	}
	p.mu.Lock()
	p.vmsByName[v.Name] = v
	p.mu.Unlock()
}

func podItems(list *corev1.PodList) []corev1.Pod {
	if list == nil {
		return nil
	}
	return list.Items
}
