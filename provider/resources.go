package provider

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
)

// enforceResources uses CH API to resize VM resources to match pod limits.
// CPU: resize vCPUs. Memory: resize balloon.
func (p *CocoonProvider) enforceResources(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	if vm.vmID == "" || strings.HasPrefix(vm.vmID, "static-") {
		return
	}
	if len(pod.Spec.Containers) == 0 {
		return
	}
	logger := log.WithFunc("provider.enforceResources")

	limits := pod.Spec.Containers[0].Resources.Limits
	if limits == nil {
		return
	}

	if cpuQ := limits.Cpu(); cpuQ != nil && !cpuQ.IsZero() { //nolint:nestif // CH API resize with validation
		desiredCPU := int(cpuQ.Value())
		if desiredCPU > 0 && desiredCPU != vm.cpu {
			sock := chSocketPath(vm.vmID)
			if sock != "" {
				body := fmt.Sprintf(`{"desired_vcpus":%d}`, desiredCPU)
				cmd := fmt.Sprintf("sudo curl -s -X PUT --unix-socket %s -H 'Content-Type: application/json' -d '%s' http://localhost/api/v1/vm.resize", sock, body)
				if out, err := exec.CommandContext(ctx, "bash", "-c", cmd).CombinedOutput(); err != nil { //nolint:gosec // cmd from trusted internal template
					logger.Debugf(ctx, "%s: CPU resize to %d failed: %v (%s)", vm.vmName, desiredCPU, err, strings.TrimSpace(string(out)))
				} else {
					logger.Infof(ctx, "%s: CPU resized to %d", vm.vmName, desiredCPU)
					vm.cpu = desiredCPU
				}
			}
		}
	}

	if memQ := limits.Memory(); memQ != nil && !memQ.IsZero() {
		desiredMB := int(memQ.Value() / (1024 * 1024))
		if desiredMB > 0 && desiredMB != vm.memoryMB {
			logger.Debugf(ctx, "%s: memory limit %dMB (VM configured %dMB, balloon resize not applied)", vm.vmName, desiredMB, vm.memoryMB)
		}
	}
}
