package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func isTerminalPodPhase(phase corev1.PodPhase) bool {
	return phase == corev1.PodFailed || phase == corev1.PodSucceeded
}

func (p *CocoonProvider) findOtherActivePodForVMID(ctx context.Context, pod *corev1.Pod, vmID string) string {
	if vmID == "" {
		return ""
	}

	selfKey := podKey(pod.Namespace, pod.Name)

	p.mu.RLock()
	for key, vm := range p.vms {
		if key == selfKey || vm == nil || vm.vmID != vmID {
			continue
		}
		if otherPod, ok := p.pods[key]; ok {
			if otherPod.DeletionTimestamp != nil || isTerminalPodPhase(otherPod.Status.Phase) {
				continue
			}
			p.mu.RUnlock()
			return key
		}
		continue
	}
	p.mu.RUnlock()

	if p.kubeClient == nil {
		return ""
	}

	opts := metav1.ListOptions{}
	if p.nodeName != "" {
		opts.FieldSelector = "spec.nodeName=" + p.nodeName
	}
	list, err := p.kubeClient.CoreV1().Pods("").List(ctx, opts)
	if err != nil {
		log.WithFunc("provider.findOtherActivePodForVMID").Warnf(ctx, "%s: list pods: %v", vmID, err)
		return ""
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Namespace == pod.Namespace && other.Name == pod.Name {
			continue
		}
		if other.DeletionTimestamp != nil || isTerminalPodPhase(other.Status.Phase) {
			continue
		}
		if ann(other, AnnVMID, "") == vmID {
			return podKey(other.Namespace, other.Name)
		}
	}
	return ""
}

func (p *CocoonProvider) GetPod(_ context.Context, ns, name string) (*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pod, ok := p.pods[podKey(ns, name)]
	if !ok {
		return nil, fmt.Errorf("pod %s/%s not found", ns, name)
	}
	return pod, nil
}

func (p *CocoonProvider) GetPods(_ context.Context) ([]*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		out = append(out, pod)
	}
	return out, nil
}

func (p *CocoonProvider) GetPodStatus(ctx context.Context, ns, name string) (*corev1.PodStatus, error) {
	key := podKey(ns, name)
	p.mu.RLock()
	vmRec, ok := p.vms[key]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("pod %s not found", key)
	}
	vm := *vmRec

	if vm.vmID != "" && !strings.HasPrefix(vm.vmID, "static-") { //nolint:nestif // VM state refresh with DHCP fallback
		if fresh := p.discoverVMByID(ctx, vm.vmID); fresh != nil {
			if fresh.vmID != "" {
				vm.vmID = fresh.vmID
			}
			if fresh.vmName != "" {
				vm.vmName = fresh.vmName
			}
			if fresh.state != "" {
				vm.state = fresh.state
			}
			if fresh.mac != "" {
				vm.mac = fresh.mac
			}
			if fresh.ip != "" {
				vm.ip = fresh.ip
			}
		}

		dhcpIP := ""
		if vm.mac != "" {
			dhcpIP = resolveIPFromLeaseByMAC(vm.mac)
		}
		if dhcpIP == "" {
			dhcpIP = p.resolveIPFromLease(vm.vmName)
		}
		if dhcpIP != "" {
			vm.ip = dhcpIP
		}

		p.syncPodRuntimeMetadata(ctx, key, &vm)
	}

	phase := corev1.PodPending
	ready := corev1.ConditionFalse
	var containerState corev1.ContainerState

	switch vm.state {
	case stateRunning:
		if vm.ip == "" {
			containerState = corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{
					Reason:  "WaitingForIP",
					Message: "VM running, waiting for DHCP IP",
				},
			}
			break
		}
		phase = corev1.PodRunning
		ready = corev1.ConditionTrue
		_, readinessOK := p.getProbeReadiness(key)
		if !readinessOK {
			ready = corev1.ConditionFalse
		}
		containerState = corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(vm.startedAt)},
		}
	case stateHibernated:
		phase = corev1.PodRunning
		containerState = corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "Hibernated",
				Message: "VM suspended to epoch, waiting for wake",
			},
		}
	case stateStopped:
		phase = corev1.PodSucceeded
		containerState = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Reason: "Stopped",
				StartedAt: metav1.NewTime(vm.startedAt), FinishedAt: metav1.Now(),
			},
		}
	case stateError:
		phase = corev1.PodFailed
		containerState = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Reason: "Error",
				StartedAt: metav1.NewTime(vm.startedAt), FinishedAt: metav1.Now(),
			},
		}
	case stateCreating:
		phase = corev1.PodPending
		containerState = corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "VMCreated"},
		}
	}

	containerName := "agent"
	p.mu.RLock()
	if pod, ok := p.pods[key]; ok && len(pod.Spec.Containers) > 0 {
		containerName = pod.Spec.Containers[0].Name
	}
	p.mu.RUnlock()

	hostIP := p.nodeIP
	if hostIP == "" {
		hostIP = vm.ip
	}
	var podIPs []corev1.PodIP
	if vm.ip != "" {
		podIPs = []corev1.PodIP{{IP: vm.ip}}
	}

	return &corev1.PodStatus{
		Phase:  phase,
		HostIP: hostIP,
		PodIP:  vm.ip,
		PodIPs: podIPs,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: ready, LastTransitionTime: metav1.Now()},
			{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
			{Type: corev1.ContainersReady, Status: ready},
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
		},
		ContainerStatuses: []corev1.ContainerStatus{
			{
				Name:         containerName,
				Ready:        vm.state == stateRunning,
				RestartCount: 0,
				Image:        vm.image,
				ImageID:      vm.image,
				ContainerID:  fmt.Sprintf("cocoon://%s", vm.vmID),
				State:        containerState,
			},
		},
	}, nil
}

// NotifyPods implements node.PodNotifier.
func (p *CocoonProvider) NotifyPods(ctx context.Context, cb func(*corev1.Pod)) {
	p.notifyPodCb = cb
	log.WithFunc("provider.NotifyPods").Info(ctx, "PodNotifier registered")
}

func (p *CocoonProvider) notifyPodStatus(ctx context.Context, ns, name string) {
	logger := log.WithFunc("provider.notifyPodStatus")
	if p.notifyPodCb == nil {
		logger.Debugf(ctx, "%s/%s: callback is nil", ns, name)
		return
	}
	p.mu.RLock()
	pod, ok := p.pods[podKey(ns, name)]
	p.mu.RUnlock()
	if !ok {
		logger.Debugf(ctx, "%s/%s: pod not found in store", ns, name)
		return
	}
	status, err := p.GetPodStatus(ctx, ns, name)
	if err != nil {
		logger.Warnf(ctx, "%s/%s: GetPodStatus: %v", ns, name, err)
		return
	}
	podCopy := pod.DeepCopy()
	podCopy.Status = *status
	logger.Infof(ctx, "%s/%s: phase=%s ready=%v containers=%d",
		ns, name, status.Phase,
		len(status.Conditions) > 0 && status.Conditions[0].Status == "True",
		len(status.ContainerStatuses))
	p.notifyPodCb(podCopy)
}
