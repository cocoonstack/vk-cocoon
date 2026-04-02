package provider

import (
	"context"
	"time"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func shouldRecoverManagedPod(mode string, pod *corev1.Pod) bool {
	if mode == modeStatic || mode == modeAdopt {
		return false
	}
	return ann(pod, AnnVMID, "") != ""
}

func shouldReuseExistingVMState(state string) bool {
	switch normalized := normalizedState(state); normalized {
	case stateStopped, stateStoppedStale, stateError, stateFailed:
		return false
	default:
		return true
	}
}

var (
	managedRecoveryAttempts = 10
	managedRecoveryInterval = time.Second
)

func (p *CocoonProvider) discoverRecoverableManagedVM(ctx context.Context, vmID, vmName string) *CocoonVM {
	var last *CocoonVM
	for attempt := range managedRecoveryAttempts {
		vm := p.lookupRecoverableVM(ctx, vmID, vmName)
		if vm != nil {
			last = vm
			if shouldReuseExistingVMState(vm.state) {
				return vm
			}
		}
		if attempt+1 >= managedRecoveryAttempts {
			break
		}
		timer := time.NewTimer(managedRecoveryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last
		case <-timer.C:
		}
	}
	return last
}

func (p *CocoonProvider) lookupRecoverableVM(ctx context.Context, vmID, vmName string) *CocoonVM {
	if vmID != "" {
		if vm := p.discoverVMByID(ctx, vmID); vm != nil {
			return vm
		}
	}
	if vmName != "" {
		return p.discoverVM(ctx, vmName)
	}
	return nil
}

func (p *CocoonProvider) patchPodAnnotations(ctx context.Context, ns, name string, annotations map[string]string) {
	if p.kubeClient == nil || len(annotations) == 0 {
		return
	}

	body, err := commonk8s.AnnotationsMergePatch(annotationPatchMap(annotations))
	if err != nil {
		log.WithFunc("provider.patchPodAnnotations").Warnf(ctx, "%s/%s: marshal patch: %v", ns, name, err)
		return
	}
	if _, err := p.kubeClient.CoreV1().Pods(ns).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		log.WithFunc("provider.patchPodAnnotations").Warnf(ctx, "%s/%s: %v", ns, name, err)
	}
}

func (p *CocoonProvider) syncPodRuntimeMetadata(ctx context.Context, key string, vm *CocoonVM) {
	if vm == nil {
		return
	}

	var (
		ns      string
		name    string
		changed map[string]string
	)

	p.mu.Lock()
	if current, ok := p.vms[key]; ok { //nolint:nestif // field-by-field merge is inherently nested
		mergeVMDetails(current, vm)
		vm = current
	}
	if pod, ok := p.pods[key]; ok {
		changed = applyVMPodAnnotations(pod, vm)
		ns = pod.Namespace
		name = pod.Name
		p.storePodVMStateLocked(key, pod, vm)
	}
	p.mu.Unlock()

	p.patchPodAnnotations(ctx, ns, name, changed)
}

func (p *CocoonProvider) recoverManagedPod(ctx context.Context, pod *corev1.Pod, key, image, osType string) bool {
	vmID := ann(pod, AnnVMID, "")
	vmName := ann(pod, AnnVMName, "")

	vm := p.discoverRecoverableManagedVM(ctx, vmID, vmName)

	if vm != nil && shouldReuseExistingVMState(vm.state) { //nolint:nestif // recovery logic requires multiple fallback checks
		if vm.vmID == "" {
			vm.vmID = vmID
		}
		if vm.vmName == "" {
			vm.vmName = vmName
		}
		if vm.ip == "" {
			if ipAnn := ann(pod, AnnIP, ""); ipAnn != "" {
				vm.ip = ipAnn
			}
		}
		if vm.ip == "" && vm.mac != "" {
			if dhcpIP := resolveIPFromLeaseByMAC(vm.mac); dhcpIP != "" {
				vm.ip = dhcpIP
			}
		}
		if vm.ip == "" && vm.vmName != "" {
			vm.ip = p.resolveIPFromLease(vm.vmName)
		}
		if vm.image == "" {
			vm.image = image
		}
		vm.podNamespace = pod.Namespace
		vm.podName = pod.Name
		vm.os = osType
		vm.managed = true
		now := time.Now()
		if vm.createdAt.IsZero() {
			vm.createdAt = now
		}
		if vm.startedAt.IsZero() {
			vm.startedAt = now
		}

		p.storePodVM(ctx, key, pod, vm)
		log.WithFunc("provider.recoverManagedPod").Infof(ctx, "CreatePod %s: recovered existing VM %s (%s) state=%s ip=%s", key, vm.vmName, vm.vmID, vm.state, vm.ip)
		go p.startProbes(ctx, pod, vm)
		go p.notifyPodStatus(ctx, pod.Namespace, pod.Name)
		return true
	}

	if ann(pod, AnnHibernate, "") == valTrue && vmName != "" {
		if ref := p.snapshotManager().lookupSuspendedSnapshot(ctx, pod.Namespace, vmName); ref != "" {
			now := time.Now()
			vm = &CocoonVM{
				podNamespace: pod.Namespace,
				podName:      pod.Name,
				vmName:       vmName,
				state:        stateHibernated,
				image:        image,
				os:           osType,
				managed:      true,
				createdAt:    now,
				startedAt:    now,
			}
			p.storePodVM(ctx, key, pod, vm)
			log.WithFunc("provider.recoverManagedPod").Infof(ctx, "CreatePod %s: recovered hibernated pod %s from snapshot %s", key, vmName, ref)
			go p.notifyPodStatus(ctx, pod.Namespace, pod.Name)
			return true
		}
	}

	if vm != nil {
		log.WithFunc("provider.recoverManagedPod").Warnf(ctx, "CreatePod %s: pod carries existing vm-id=%s vm-name=%s but VM stayed in non-recoverable state=%s after retry; continuing with create", key, vmID, vmName, vm.state)
		return false
	}
	log.WithFunc("provider.recoverManagedPod").Warnf(ctx, "CreatePod %s: pod carries existing vm-id=%s vm-name=%s but no recoverable VM was found; continuing with create", key, vmID, vmName)
	return false
}

func annotationPatchMap(annotations map[string]string) map[string]any {
	patch := make(map[string]any, len(annotations))
	for key, value := range annotations {
		patch[key] = value
	}
	return patch
}
