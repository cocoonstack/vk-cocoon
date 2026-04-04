package provider

import (
	"context"

	"github.com/projecteru2/core/log"
)

// forkFromMainAgent creates a live snapshot of the main agent (slot-0) VM
// and returns the snapshot name for the sub-agent to clone from.
// Returns "" if the main agent is not running or snapshot fails.
func (p *CocoonProvider) forkFromMainAgent(ctx context.Context, _, vmName string) string {
	return p.forkFromSource(ctx, mainAgentVMName(vmName), vmName, "provider.forkFromMainAgent")
}

// forkFromVM creates a live snapshot of a specific source VM.
// Used by CocoonSet controller via cocoon.cis/fork-from annotation.
func (p *CocoonProvider) forkFromVM(ctx context.Context, _, sourceVMName, targetVMName string) string {
	return p.forkFromSource(ctx, sourceVMName, targetVMName, "provider.forkFromVM")
}

func (p *CocoonProvider) forkFromSource(ctx context.Context, sourceVMName, targetVMName, loggerFunc string) string {
	logger := log.WithFunc(loggerFunc)
	snapshots := p.snapshotManager()

	sourceVM := p.findRunningVMByName(sourceVMName)
	if sourceVM == nil {
		logger.Warnf(ctx, "source VM %s not running, falling back to base image", sourceVMName)
		return ""
	}

	forkSnap := targetVMName + "-fork"
	out, err := snapshots.saveSnapshot(ctx, forkSnap, sourceVM.vmID)
	if err != nil {
		logger.Errorf(ctx, err, "snapshot %s failed: %s", sourceVMName, out)
		return ""
	}
	logger.Infof(ctx, "live snapshot of %s (%s) -> %s", sourceVMName, sourceVM.vmID, forkSnap)
	return forkSnap
}

func (p *CocoonProvider) findRunningVMByName(name string) *CocoonVM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, vm := range p.vms {
		if vm.vmName == name && vm.state == stateRunning && vm.vmID != "" {
			return vm
		}
	}
	return nil
}
