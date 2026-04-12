package main

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
	"github.com/cocoonstack/vk-cocoon/vm"
)

// StartupReconcile rebuilds the in-memory tables from K8s pods and
// cocoon VMs so restarts don't leak VMs or lose pod associations.
// Unmatched VMs are handled per OrphanPolicy.
func (p *CocoonProvider) StartupReconcile(ctx context.Context) error {
	logger := log.WithFunc("CocoonProvider.StartupReconcile")
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

	if pods != nil {
		for i := range pods.Items {
			pod := &pods.Items[i]
			runtime := meta.ParseVMRuntime(pod)
			if runtime.VMID == "" {
				continue
			}
			idx, ok := vmByID[runtime.VMID]
			if !ok {
				logger.Warnf(ctx, "pod %s/%s annotates VMID %s but no such VM exists locally; CreatePod will recreate",
					pod.Namespace, pod.Name, runtime.VMID)
				continue
			}
			v := vms[idx]
			p.trackPod(pod, &v)
			matched[v.ID] = true
			if p.Probes != nil {
				p.Probes.Start(
					meta.PodKey(pod.Namespace, pod.Name),
					p.buildProbe(pod.Namespace, pod.Name),
					p.buildOnUpdate(pod.Namespace, pod.Name),
				)
			}
		}
	}

	for i := range vms {
		if matched[vms[i].ID] {
			continue
		}
		p.handleOrphan(ctx, &vms[i])
	}

	metrics.VMTableSize.Set(float64(len(matched)))
	logger.Infof(ctx, "startup reconcile: %d pods adopted, %d orphan VMs", len(matched), len(vms)-len(matched))
	return nil
}

// handleOrphan applies OrphanPolicy to an unmatched VM.
func (p *CocoonProvider) handleOrphan(ctx context.Context, v *vm.VM) {
	logger := log.WithFunc("CocoonProvider.handleOrphan")
	switch p.OrphanPolicy {
	case OrphanDestroy:
		logger.Warnf(ctx, "destroying orphan VM %s (id=%s)", v.Name, v.ID)
		if err := p.Runtime.Remove(ctx, v.ID); err != nil {
			logger.Errorf(ctx, err, "remove orphan VM %s", v.ID)
		}
	case OrphanKeep:
		// no-op
	default: // OrphanAlert
		metrics.OrphanVMTotal.Inc()
		logger.Warnf(ctx, "orphan VM detected: name=%s id=%s state=%s ip=%s — apply policy=destroy to clean up automatically",
			v.Name, v.ID, v.State, v.IP)
	}
}
