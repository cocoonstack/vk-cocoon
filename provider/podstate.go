package provider

import "context"

import corev1 "k8s.io/api/core/v1"

const (
	modeClone  = "clone"
	modeRun    = "run"
	modeStatic = "static"
	modeAdopt  = "adopt"

	stateUnknown      = "unknown"
	stateCreating     = "creating"
	stateSuspending   = "suspending"
	stateStoppedStale = "stopped (stale)"
	stateFailed       = "failed"
	stateError        = "error"
)

type podAnnotation struct {
	key   string
	value string
}

// setPodAnnotation sets an annotation if the value is non-empty and records
// the changed value in the provided map.
func setPodAnnotation(pod *corev1.Pod, changed map[string]string, key, value string) {
	if value == "" {
		return
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	if pod.Annotations[key] == value {
		return
	}
	pod.Annotations[key] = value
	if changed != nil {
		changed[key] = value
	}
}

// applyVMPodAnnotations updates the pod object with the VM identity/status
// annotations that the provider owns.
func applyVMPodAnnotations(pod *corev1.Pod, vm *CocoonVM) map[string]string {
	changed := map[string]string{}
	setPodAnnotation(pod, changed, AnnVMName, vm.vmName)
	setPodAnnotation(pod, changed, AnnVMID, vm.vmID)
	setPodAnnotation(pod, changed, AnnIP, vm.ip)
	setPodAnnotation(pod, changed, AnnMAC, vm.mac)
	if vm.managed {
		setPodAnnotation(pod, changed, AnnManaged, valTrue)
	}
	return changed
}

// storePodVMState stores a deep copy of the pod and the VM pointer in the
// provider's in-memory maps.
func (p *CocoonProvider) storePodVMState(key string, pod *corev1.Pod, vm *CocoonVM) {
	p.mu.Lock()
	p.storePodVMStateLocked(key, pod, vm)
	p.mu.Unlock()
}

func (p *CocoonProvider) storePodVMStateLocked(key string, pod *corev1.Pod, vm *CocoonVM) {
	p.pods[key] = pod.DeepCopy()
	p.vms[key] = vm
}

// storePodVM updates annotations, persists the in-memory state, and patches the
// live Pod object with the changed annotations.
func (p *CocoonProvider) storePodVM(ctx context.Context, key string, pod *corev1.Pod, vm *CocoonVM, extras ...podAnnotation) {
	changed := applyVMPodAnnotations(pod, vm)
	for _, extra := range extras {
		setPodAnnotation(pod, changed, extra.key, extra.value)
	}
	p.storePodVMState(key, pod, vm)
	p.patchPodAnnotations(ctx, pod.Namespace, pod.Name, changed)
}

// mergeVMDetails copies non-empty fields from src into dst.
func mergeVMDetails(dst, src *CocoonVM) {
	if dst == nil || src == nil {
		return
	}
	if src.vmID != "" {
		dst.vmID = src.vmID
	}
	if src.vmName != "" {
		dst.vmName = src.vmName
	}
	if src.ip != "" {
		dst.ip = src.ip
	}
	if src.mac != "" {
		dst.mac = src.mac
	}
	if src.state != "" {
		dst.state = src.state
	}
	if src.image != "" {
		dst.image = src.image
	}
	if src.os != "" {
		dst.os = src.os
	}
	if src.managed {
		dst.managed = true
	}
	if src.cpu != 0 {
		dst.cpu = src.cpu
	}
	if src.memoryMB != 0 {
		dst.memoryMB = src.memoryMB
	}
	if !src.createdAt.IsZero() {
		dst.createdAt = src.createdAt
	}
	if !src.startedAt.IsZero() {
		dst.startedAt = src.startedAt
	}
}
