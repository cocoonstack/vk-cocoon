package provider

import (
	"context"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type managedVMSnapshot struct {
	key  string
	vmID string
	name string
}

// reconcileLoop periodically checks all managed VMs and refreshes any that still exist.
func (p *CocoonProvider) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.reconcileOnce(ctx)
		}
	}
}

func (p *CocoonProvider) reconcileOnce(ctx context.Context) {
	p.reconcileHibernateAnnotations(ctx)

	p.mu.RLock()
	var managed []managedVMSnapshot
	for key, vm := range p.vms {
		if vm.managed {
			managed = append(managed, managedVMSnapshot{key: key, vmID: vm.vmID, name: vm.vmName})
		}
	}
	p.mu.RUnlock()

	for _, snap := range managed {
		updated, ok := p.reconciledVMState(ctx, snap)
		if !ok {
			continue
		}

		p.syncPodRuntimeMetadata(ctx, snap.key, updated)
		if updated.state != stateRunning {
			log.WithFunc("provider.reconcileOnce").Warnf(ctx, "VM %s (%s) state=%s (not auto-restarting — VMs are ephemeral now)",
				snap.name, snap.vmID, updated.state)
		}

		// Re-notify pod status so the VK framework can patch the API server.
		// The initial notify from CreatePod may be lost if the VK informer
		// has not yet synced the pod into knownPods.
		if ns, name, ok := strings.Cut(snap.key, "/"); ok {
			go p.notifyPodStatus(ctx, ns, name)
		}
	}

	p.reconcileOrphanedPods(ctx)
}

// reconcileOrphanedPods detects pods whose VM no longer exists and notifies
// the VK framework with a terminal phase (Succeeded/Failed).  This handles
// the case where a pod is stuck in Terminating because the VK framework only
// calls DeletePod for Running pods — if the VM disappeared (or was never
// created) while the pod was Pending, the framework never finalizes it.
func (p *CocoonProvider) reconcileOrphanedPods(ctx context.Context) {
	logger := log.WithFunc("provider.reconcileOrphanedPods")

	p.mu.RLock()
	type orphanCandidate struct {
		key  string
		ns   string
		name string
	}
	var candidates []orphanCandidate
	for key, vm := range p.vms {
		if vm == nil || !vm.managed {
			continue
		}
		if vm.state == stateStopped || vm.state == stateError || vm.state == stateFailed || vm.state == stateStoppedStale {
			if ns, name, ok := strings.Cut(key, "/"); ok {
				candidates = append(candidates, orphanCandidate{key: key, ns: ns, name: name})
			}
		}
	}
	p.mu.RUnlock()

	for _, c := range candidates {
		// Check if the pod is being deleted (DeletionTimestamp set) in the API server.
		if p.kubeClient == nil {
			continue
		}
		apiPod, err := p.kubeClient.CoreV1().Pods(c.ns).Get(ctx, c.name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if apiPod.DeletionTimestamp == nil {
			continue
		}

		// Pod is being deleted but VK framework can't finalize because the provider
		// never reported a terminal status.  Force-delete via API.
		logger.Infof(ctx, "%s: VM gone and pod has DeletionTimestamp, force-deleting orphaned pod", c.key)
		gracePeriod := int64(0)
		_ = p.kubeClient.CoreV1().Pods(c.ns).Delete(ctx, c.name, metav1.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
		})
		p.cleanupDeletedPod(c.key, c.name)
	}
}

func (p *CocoonProvider) reconciledVMState(ctx context.Context, snap managedVMSnapshot) (*CocoonVM, bool) {
	p.mu.RLock()
	vmRec, ok := p.vms[snap.key]
	if ok && (vmRec.state == stateHibernated || vmRec.state == stateCreating) {
		p.mu.RUnlock()
		return nil, false
	}
	if !ok {
		p.mu.RUnlock()
		return nil, false
	}
	updated := *vmRec
	p.mu.RUnlock()

	fresh := p.lookupRecoverableVM(ctx, snap.vmID, snap.name)
	if fresh == nil {
		log.WithFunc("provider.reconcileOnce").Warnf(ctx, "VM %s (%s) not found by cocoon, marking stopped", snap.name, snap.vmID)
		updated.state = stateStopped
		return &updated, true
	}

	if snap.vmID == "" && fresh.vmID != "" {
		updated.vmID = fresh.vmID
	}
	if fresh.vmName != "" {
		updated.vmName = fresh.vmName
	}
	if fresh.state != "" {
		updated.state = fresh.state
	}
	if fresh.mac != "" {
		updated.mac = fresh.mac
	}
	if fresh.ip != "" {
		updated.ip = fresh.ip
	}
	if updated.mac != "" {
		if dhcpIP := resolveIPFromLeaseByMAC(updated.mac); dhcpIP != "" {
			updated.ip = dhcpIP
		}
	}
	return &updated, true
}

// getPuller returns an EpochPuller for the given registry URL, creating one on demand.
func (p *CocoonProvider) getPuller(ctx context.Context, registryURL string) *EpochPuller {
	if registryURL == "" {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if ep, ok := p.pullers[registryURL]; ok {
		return ep
	}

	rootDir := cocoonRootDir()
	ep := NewEpochPuller(registryURL, rootDir, p.cocoonBin)
	p.pullers[registryURL] = ep
	log.WithFunc("provider.getPuller").Infof(ctx, "epoch puller created for %s (root=%s)", registryURL, rootDir)
	return ep
}
