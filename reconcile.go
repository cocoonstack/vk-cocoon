package main

import (
	"context"
	"fmt"

	"github.com/projecteru2/core/log"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// StartupReconcile rebuilds the in-memory pod / VM tables from the
// live cluster state and the cocoon runtime so vk-cocoon can be
// stopped and restarted without leaking VMs or losing pod
// associations. K8s API is the source of truth; cocoon vm list is
// the cross-check.
//
// Algorithm:
//
//  1. List every pod scheduled to NodeName via fieldSelector.
//  2. Ask the Runtime for every VM currently running on the host.
//  3. For each pod with a VMID annotation, look up the VM by ID
//     and adopt it into the in-memory tables.
//  4. Any pod missing a VMID annotation is left alone — the
//     CreatePod path will pick it up.
//  5. Any VM not matched to a pod is an orphan; apply OrphanPolicy.
func (p *CocoonProvider) StartupReconcile(ctx context.Context) error {
	logger := log.WithFunc("CocoonProvider.StartupReconcile")
	if p.Clientset == nil {
		return fmt.Errorf("clientset is required for startup reconcile")
	}

	pods, err := p.Clientset.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + p.NodeName,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("list pods on %s: %w", p.NodeName, err)
	}

	vms, err := p.Runtime.List(ctx)
	if err != nil {
		return fmt.Errorf("list local VMs: %w", err)
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
				p.Probes.MarkReady(podKey(pod.Namespace, pod.Name))
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

// handleOrphan applies the configured OrphanPolicy to a VM that
// startup reconcile could not match to any pod.
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
