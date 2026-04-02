package provider

import (
	"context"

	"github.com/projecteru2/core/log"
)

// forkFromMainAgent creates a live snapshot of the main agent (slot-0) VM
// and returns the snapshot name for the sub-agent to clone from.
// Returns "" if the main agent is not running or snapshot fails.
func (p *CocoonProvider) forkFromMainAgent(ctx context.Context, _, vmName string) string {
	logger := log.WithFunc("provider.forkFromMainAgent")
	mainVM := mainAgentVMName(vmName)
	snapshots := p.snapshotManager()

	p.mu.RLock()
	var sourceVM *CocoonVM
	for _, vm := range p.vms {
		if vm.vmName == mainVM && vm.state == stateRunning && vm.vmID != "" {
			sourceVM = vm
			break
		}
	}
	p.mu.RUnlock()

	if sourceVM == nil {
		logger.Warnf(ctx, "main agent VM %s not running, falling back to base image", mainVM)
		return ""
	}

	forkSnap := vmName + "-fork"
	out, err := snapshots.saveSnapshot(ctx, forkSnap, sourceVM.vmID)
	if err != nil {
		logger.Errorf(ctx, err, "snapshot %s failed: %s", mainVM, out)
		return ""
	}
	logger.Infof(ctx, "live snapshot of %s (%s) -> %s", mainVM, sourceVM.vmID, forkSnap)
	return forkSnap
}

// forkFromVM creates a live snapshot of a specific source VM.
// Used by CocoonSet controller via cocoon.cis/fork-from annotation.
func (p *CocoonProvider) forkFromVM(ctx context.Context, _, sourceVMName, targetVMName string) string {
	logger := log.WithFunc("provider.forkFromVM")
	snapshots := p.snapshotManager()

	p.mu.RLock()
	var sourceVM *CocoonVM
	for _, vm := range p.vms {
		if vm.vmName == sourceVMName && vm.state == stateRunning && vm.vmID != "" {
			sourceVM = vm
			break
		}
	}
	p.mu.RUnlock()

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
