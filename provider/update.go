package provider

import (
	"context"

	corev1 "k8s.io/api/core/v1"
)

func (p *CocoonProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	key := podKey(pod.Namespace, pod.Name)

	p.mu.RLock()
	oldPod := p.pods[key]
	vm := p.vms[key]
	p.mu.RUnlock()

	wasHibernated := oldPod != nil && ann(oldPod, AnnHibernate, "") == valTrue
	wantHibernate := ann(pod, AnnHibernate, "") == valTrue

	storedPod := p.storeUpdatedPod(key, pod)

	if p.triggerHibernateTransition(ctx, storedPod, vm, wasHibernated, wantHibernate) {
		return nil
	}

	p.reinjectUpdatedPod(ctx, key, storedPod)
	return nil
}

func (p *CocoonProvider) storeUpdatedPod(key string, pod *corev1.Pod) *corev1.Pod {
	storedPod := pod.DeepCopy()
	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, ok := p.pods[key]; ok {
		if storedPod.Annotations == nil {
			storedPod.Annotations = map[string]string{}
		}
		for _, annotationKey := range []string{AnnVMName, AnnVMID, AnnIP, AnnMAC, AnnManaged, AnnSnapshotFrom} {
			if value, ok := existing.Annotations[annotationKey]; ok {
				storedPod.Annotations[annotationKey] = value
			}
		}
	}
	p.pods[key] = storedPod
	return storedPod
}

func (p *CocoonProvider) triggerHibernateTransition(ctx context.Context, pod *corev1.Pod, vm *CocoonVM, wasHibernated, wantHibernate bool) bool {
	if vm == nil {
		return false
	}
	if !wasHibernated && wantHibernate && vm.state == stateRunning {
		go p.hibernateVM(ctx, pod, vm)
		return true
	}
	if wasHibernated && !wantHibernate && vm.state == stateHibernated {
		go p.wakeVM(ctx, pod, vm)
		return true
	}
	return false
}

func (p *CocoonProvider) reinjectUpdatedPod(ctx context.Context, key string, pod *corev1.Pod) {
	p.mu.RLock()
	vm := p.vms[key]
	p.mu.RUnlock()
	if vm != nil && vm.os != osWindows && vm.ip != "" && vm.state == stateRunning {
		go p.postBootInject(ctx, pod, vm)
	}
}
