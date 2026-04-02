package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
)

// allocateSlotLocked finds the first available replica slot. Caller must hold p.mu.
func (p *CocoonProvider) allocateSlotLocked(ns, deployName string) int {
	prefix := fmt.Sprintf("vk-%s-%s-", ns, deployName)

	usedSlots := make(map[int]bool)
	maxSlot := -1
	for _, vm := range p.vms {
		if strings.HasPrefix(vm.vmName, prefix) {
			if vm.state == stateSuspending || vm.state == stateHibernated {
				continue
			}
			slotStr := vm.vmName[len(prefix):]
			if slot, err := strconv.Atoi(slotStr); err == nil {
				usedSlots[slot] = true
				if slot > maxSlot {
					maxSlot = slot
				}
			}
		}
	}

	for i := range maxSlot + 1 {
		if !usedSlots[i] {
			return i
		}
	}
	return maxSlot + 1
}

// deriveStableVMNameLocked resolves a stable VM name. Caller must hold p.mu (write lock).
// This ensures slot allocation + reservation is atomic.
func (p *CocoonProvider) deriveStableVMNameLocked(ctx context.Context, pod *corev1.Pod) string {
	if name := ann(pod, AnnVMName, ""); name != "" {
		return name
	}

	if deployName := p.getOwnerDeploymentName(ctx, pod); deployName != "" {
		slot := p.allocateSlotLocked(pod.Namespace, deployName)
		return meta.VMNameForDeployment(pod.Namespace, deployName, slot)
	}

	return meta.VMNameForPod(pod.Namespace, pod.Name)
}

// extractSlotFromVMName extracts the slot number from a Deployment VM name.
// "vk-prod-deploy-2" -> 2. Returns -1 for non-Deployment names.
func extractSlotFromVMName(vmName string) int {
	return meta.ExtractSlotFromVMName(vmName)
}

// mainAgentVMName derives the slot-0 VM name from any slot's VM name.
// "vk-prod-deploy-2" -> "vk-prod-deploy-0"
func mainAgentVMName(vmName string) string {
	return meta.MainAgentVMName(vmName)
}

// isMainAgent returns true if the VM name represents slot-0 (the main agent).
func isMainAgent(vmName string) bool {
	return meta.InferRoleFromVMName(vmName) == meta.RoleMain
}
