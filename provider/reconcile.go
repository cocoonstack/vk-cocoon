package provider

import (
	"context"
	"os"
	"time"

	"github.com/projecteru2/core/log"
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
	}
}

func (p *CocoonProvider) reconciledVMState(ctx context.Context, snap managedVMSnapshot) (*CocoonVM, bool) {
	p.mu.RLock()
	vmRec, ok := p.vms[snap.key]
	if ok && vmRec.state == stateHibernated {
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

	rootDir := os.Getenv("COCOON_ROOT")
	if rootDir == "" {
		rootDir = "/var/lib/cocoon"
	}
	ep := NewEpochPuller(registryURL, rootDir, p.cocoonBin)
	p.pullers[registryURL] = ep
	log.WithFunc("provider.getPuller").Infof(ctx, "epoch puller created for %s (root=%s)", registryURL, rootDir)
	return ep
}
