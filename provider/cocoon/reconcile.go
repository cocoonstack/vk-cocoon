package cocoon

import (
	"context"
	"errors"
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

// StartupReconcile rebuilds the in-memory tables from K8s pods and
// cocoon VMs so restarts don't leak VMs or lose pod associations.
// Unmatched VMs are handled per OrphanPolicy.
func (p *Provider) StartupReconcile(ctx context.Context) error {
	logger := log.WithFunc("Provider.StartupReconcile")
	if p.Clientset == nil {
		return errors.New("clientset is required for startup reconcile")
	}

	// Fetch pods and VMs concurrently (independent backends).
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

	vmByID := make(map[string]int, len(vms))
	for i := range vms {
		vmByID[vms[i].ID] = i
	}
	matched := make(map[string]bool, len(vms))

	for i := range podItems(pods) {
		pod := &pods.Items[i]
		runtime := meta.ParseVMRuntime(pod)
		if runtime.VMID == "" {
			p.reconcileNoVMID(ctx, pod)
			continue
		}
		idx, ok := vmByID[runtime.VMID]
		if !ok {
			// Hibernated pod with stale VMID — the VM was removed during
			// hibernate but the annotation patch failed. Clear the stale
			// annotations and track the pod without a VM so wake works.
			if meta.ReadHibernateState(pod) {
				p.reconcileStaleHibernate(ctx, pod)
				continue
			}
			logger.Warnf(ctx, "pod %s/%s annotates VMID %s but no such VM exists locally; CreatePod will recreate",
				pod.Namespace, pod.Name, runtime.VMID)
			continue
		}
		v := vms[idx]
		p.trackPod(pod, &v)
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

// podItems returns the Items slice from a PodList, or nil if the list is nil.
func podItems(list *corev1.PodList) []corev1.Pod {
	if list == nil {
		return nil
	}
	return list.Items
}

// reconcileStaleHibernate clears stale VMID/IP annotations from a
// hibernated pod whose VM was already removed. This happens when the
// annotation patch in hibernate() failed — the pod kept stale runtime
// annotations. We patch them away so wake can proceed normally.
func (p *Provider) reconcileStaleHibernate(ctx context.Context, pod *corev1.Pod) {
	logger := log.WithFunc("Provider.reconcileStaleHibernate")
	logger.Infof(ctx, "pod %s/%s is hibernated with stale VMID, clearing annotations", pod.Namespace, pod.Name)
	if err := p.clearRuntimeAnnotations(ctx, pod); err != nil {
		logger.Errorf(ctx, err, "clear stale hibernate annotations %s/%s", pod.Namespace, pod.Name)
	}
	p.trackPod(pod, nil)
}

// reconcileNoVMID handles a pod with no VMID during startup reconcile.
// Hibernated pods are tracked without a VM; others are skipped.
func (p *Provider) reconcileNoVMID(ctx context.Context, pod *corev1.Pod) {
	if !meta.ReadHibernateState(pod) {
		return
	}
	p.trackPod(pod, nil)
	log.WithFunc("Provider.StartupReconcile").
		Infof(ctx, "pod %s/%s hibernated, tracking without VM", pod.Namespace, pod.Name)
}

// handleOrphan applies OrphanPolicy to an unmatched VM.
func (p *Provider) handleOrphan(ctx context.Context, v *vm.VM) {
	logger := log.WithFunc("Provider.handleOrphan")
	switch p.OrphanPolicy {
	case provider.OrphanDestroy:
		logger.Warnf(ctx, "destroying orphan VM %s (id=%s)", v.Name, v.ID)
		if err := p.Runtime.Remove(ctx, v.ID); err != nil {
			logger.Errorf(ctx, err, "remove orphan VM %s", v.ID)
		}
	case provider.OrphanKeep:
		// no-op
	default: // provider.OrphanAlert
		metrics.OrphanVMTotal.Inc()
		logger.Warnf(ctx, "orphan VM detected: name=%s id=%s state=%s ip=%s — apply policy=destroy to clean up automatically",
			v.Name, v.ID, v.State, v.IP)
	}
}
