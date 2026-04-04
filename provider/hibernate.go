package provider

import (
	"context"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (p *CocoonProvider) reconcileHibernateAnnotations(ctx context.Context) {
	logger := log.WithFunc("provider.reconcileHibernateAnnotations")

	p.mu.RLock()
	type check struct {
		key string
		pod *corev1.Pod
		vm  *CocoonVM
	}
	var checks []check
	for key, pod := range p.pods {
		vm := p.vms[key]
		if vm == nil || !vm.managed {
			continue
		}
		checks = append(checks, check{key: key, pod: pod, vm: vm})
	}
	p.mu.RUnlock()

	if len(checks) == 0 || p.kubeClient == nil {
		return
	}

	opts := metav1.ListOptions{}
	if p.nodeName != "" {
		opts.FieldSelector = "spec.nodeName=" + p.nodeName
	}
	list, err := p.kubeClient.CoreV1().Pods("").List(ctx, opts)
	if err != nil {
		logger.Warnf(ctx, "list pods for hibernate reconcile: %v", err)
		return
	}
	liveAnnotations := make(map[string]string, len(list.Items))
	for i := range list.Items {
		lp := &list.Items[i]
		liveAnnotations[podKey(lp.Namespace, lp.Name)] = lp.Annotations[AnnHibernate]
	}

	for _, c := range checks {
		liveHibernate := liveAnnotations[c.key]

		if liveHibernate == valTrue && c.vm.state == stateRunning {
			logger.Infof(ctx, "%s has hibernate=true, triggering", c.key)
			p.hibernateVM(ctx, c.pod, c.vm)
		} else if liveHibernate != valTrue && c.vm.state == stateHibernated {
			logger.Infof(ctx, "%s hibernate removed, triggering wake", c.key)
			p.wakeVM(ctx, c.pod, c.vm)
		}
	}
}

func (p *CocoonProvider) hibernateVM(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	logger := log.WithFunc("provider.hibernateVM")
	key := podKey(pod.Namespace, pod.Name)

	if isMainAgent(vm.vmName) {
		logger.Warnf(ctx, "%s: rejected, slot-0 cannot be hibernated", key)
		return
	}

	logger.Infof(ctx, "%s: starting (vm=%s id=%s)", key, vm.vmName, vm.vmID)
	p.stopProbes(key)

	spec := resolvePodSpec(pod)
	puller := p.getPuller(ctx, spec.registryURL)
	snapshots := p.snapshotManager()

	fullRef, err := snapshots.suspendVM(ctx, pod, vm, spec.registryURL, puller)
	if err != nil {
		logger.Errorf(ctx, err, "%s: snapshot failed", key)
		return
	}
	logger.Infof(ctx, "%s: suspended snapshot recorded as %s", key, fullRef)

	p.removeVM(ctx, vm.vmID)
	p.podMap.Delete(key)

	p.mu.Lock()
	vm.state = stateHibernated
	vm.vmID = ""
	vm.ip = ""
	p.mu.Unlock()

	logger.Infof(ctx, "%s: complete, pod stays alive", key)
	go p.notifyPodStatus(ctx, pod.Namespace, pod.Name)
}

func (p *CocoonProvider) wakeVM(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	logger := log.WithFunc("provider.wakeVM")
	key := podKey(pod.Namespace, pod.Name)
	logger.Infof(ctx, "%s: starting (vm=%s)", key, vm.vmName)

	spec := resolvePodSpec(pod)
	registryURL := spec.registryURL
	cloneImage := spec.cloneImage()
	snapshots := p.snapshotManager()

	if suspended, ok := snapshots.consumeSuspendedSnapshot(ctx, pod.Namespace, vm.vmName, false); ok {
		logger.Infof(ctx, "%s: found suspended snapshot %s", key, suspended.ref)
		cloneImage = suspended.snapshot
		if suspended.registryURL != "" {
			registryURL = suspended.registryURL
		}
	}

	puller := p.getPuller(ctx, registryURL)
	if puller != nil {
		if err := puller.EnsureSnapshot(ctx, cloneImage); err != nil {
			logger.Warnf(ctx, "%s: epoch pull %s failed: %v", key, cloneImage, err)
		}
	}

	cpu, mem := podResourceLimits(pod)
	storage := spec.storage

	if existing := p.discoverVM(ctx, vm.vmName); existing != nil && existing.vmID != "" {
		p.removeVM(ctx, existing.vmID)
	}

	out, err := p.cocoonExec(ctx, buildCloneArgs(vm.vmName, spec.network, cloneImage)...)
	if err != nil {
		resolved := resolveCloneBootImage(cloneImage)
		if resolved == cloneImage {
			logger.Errorf(ctx, err, "%s: clone failed: %s", key, out)
			return
		}
		logger.Warnf(ctx, "%s: cocoon vm clone failed, falling back to cold boot from %s", key, resolved)
		out, err = p.cocoonExec(ctx, buildRunArgs(runConfig{
			vmName:  vm.vmName,
			cpu:     cpu,
			mem:     mem,
			storage: storage,
			image:   resolved,
			osType:  spec.osType,
			network: spec.network,
		})...)
		if err != nil {
			logger.Errorf(ctx, err, "%s: clone fallback failed: %s", key, out)
			return
		}
	}

	vmID := parseVMID(out)
	var fresh *CocoonVM
	if vmID != "" {
		fresh = p.awaitVMInCache(ctx, vmID, vm.vmName, 10*time.Second)
	}
	if fresh == nil {
		fresh = p.discoverCreatedVM(ctx, vm.vmName, vmID)
	}
	if fresh == nil {
		logger.Warnf(ctx, "%s: VM not found after clone", key)
		return
	}

	fresh.podNamespace = pod.Namespace
	fresh.podName = pod.Name
	fresh.os = spec.osType
	fresh.managed = true
	fresh.ip = p.waitForDHCPIP(ctx, fresh, 120*time.Second)

	p.mu.Lock()
	vm.vmID = fresh.vmID
	vm.state = stateRunning
	vm.ip = fresh.ip
	vm.mac = fresh.mac
	vm.startedAt = time.Now()
	p.mu.Unlock()

	p.syncPodRuntimeMetadata(ctx, key, fresh)
	snapshots.clearSuspendedSnapshot(ctx, pod.Namespace, vm.vmName)

	go p.postBootInject(ctx, pod, vm)
	go p.startProbes(ctx, pod, vm)

	p.podMap.Store(key, fresh.vmID, fresh.vmName, fresh.image)
	logger.Infof(ctx, "%s: complete (ip=%s)", key, fresh.ip)
	go p.notifyPodStatus(ctx, pod.Namespace, pod.Name)
}
