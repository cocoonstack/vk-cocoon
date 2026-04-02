// Package provider — hibernate/wake lifecycle for pods.
//
// Hibernate: snapshot running VM -> push to epoch -> destroy VM -> pod stays Running+NotReady.
// Wake: pull snapshot from epoch -> clone VM -> pod becomes Running+Ready.
//
// Triggered by the cocoon.cis/hibernate annotation:
//
//	annotation set to "true"  -> hibernateVM (via reconcile loop)
//	annotation removed        -> wakeVM     (via reconcile loop)
//
// The pod object stays alive in K8s throughout the hibernate/wake cycle,
// so Deployment/StatefulSet controllers don't recreate it.
package provider

import (
	"context"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// reconcileHibernateAnnotations checks all tracked pods for hibernate annotation
// changes. Called from the 30s reconcile loop because the VK framework doesn't
// trigger UpdatePod for annotation-only changes.
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

	for _, c := range checks {
		// Read the LIVE annotation from K8s (p.pods may be stale).
		livePod, err := p.kubeClient.CoreV1().Pods(c.pod.Namespace).Get(ctx, c.pod.Name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		liveHibernate := livePod.Annotations[AnnHibernate]

		if liveHibernate == valTrue && c.vm.state == stateRunning {
			logger.Infof(ctx, "%s has hibernate=true, triggering", c.key)
			p.hibernateVM(ctx, c.pod, c.vm)
		} else if liveHibernate != valTrue && c.vm.state == stateHibernated {
			logger.Infof(ctx, "%s hibernate removed, triggering wake", c.key)
			p.wakeVM(ctx, c.pod, c.vm)
		}
	}
}

// hibernateVM snapshots a running VM, pushes to epoch, and destroys the VM.
// The pod stays alive with phase=Running, Ready=False (container Waiting "Hibernated").
func (p *CocoonProvider) hibernateVM(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	logger := log.WithFunc("provider.hibernateVM")
	key := podKey(pod.Namespace, pod.Name)

	// Slot-0 is the main agent — cannot be hibernated.
	if isMainAgent(vm.vmName) {
		logger.Warnf(ctx, "%s: REJECTED — slot-0 (main agent) cannot be hibernated", key)
		return
	}

	logger.Infof(ctx, "%s: starting (vm=%s id=%s)", key, vm.vmName, vm.vmID)

	// Stop probes — VM will be gone.
	p.stopProbes(key)

	// Resolve epoch registry URL.
	spec := resolvePodSpec(pod)
	puller := p.getPuller(ctx, spec.registryURL)
	snapshots := p.snapshotManager()

	// 1. Snapshot the running VM and record the restore ref.
	fullRef, err := snapshots.suspendVM(ctx, pod, vm, spec.registryURL, puller)
	if err != nil {
		logger.Errorf(ctx, err, "%s: snapshot failed", key)
		return
	}
	logger.Infof(ctx, "%s: suspended snapshot recorded as %s", key, fullRef)

	// 3. Destroy VM.
	p.removeVM(ctx, vm.vmID)

	// 4. Mark as hibernated (pod stays, VM gone).
	p.mu.Lock()
	vm.state = stateHibernated
	vm.vmID = ""
	vm.ip = ""
	p.mu.Unlock()

	logger.Infof(ctx, "%s: complete — pod stays alive, VM destroyed", key)
	go p.notifyPodStatus(ctx, pod.Namespace, pod.Name)
}

// wakeVM restores a hibernated VM from its epoch snapshot.
// Triggered when the cocoon.cis/hibernate annotation is removed.
func (p *CocoonProvider) wakeVM(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	logger := log.WithFunc("provider.wakeVM")
	key := podKey(pod.Namespace, pod.Name)
	logger.Infof(ctx, "%s: starting (vm=%s)", key, vm.vmName)

	// Resolve image and check for suspended snapshot.
	spec := resolvePodSpec(pod)
	registryURL := spec.registryURL
	cloneImage := spec.cloneImage()
	snapshots := p.snapshotManager()

	if suspended, ok := snapshots.consumeSuspendedSnapshot(ctx, pod.Namespace, vm.vmName, true); ok {
		logger.Infof(ctx, "%s: found suspended snapshot %s", key, suspended.ref)
		cloneImage = suspended.snapshot
		if suspended.registryURL != "" {
			registryURL = suspended.registryURL
		}
	}

	// Pull from epoch.
	puller := p.getPuller(ctx, registryURL)
	if puller != nil {
		if err := puller.EnsureSnapshot(ctx, cloneImage); err != nil {
			logger.Warnf(ctx, "%s: epoch pull %s failed: %v", key, cloneImage, err)
		}
	}
	if resolved := resolveCloneBootImage(cloneImage); resolved != cloneImage {
		logger.Infof(ctx, "%s: resolved wake snapshot %s -> %s", key, cloneImage, resolved)
		cloneImage = resolved
	}

	// Resource limits.
	cpu, mem := podResourceLimits(pod)
	storage := spec.storage

	// Clean up any stale VM with same name.
	if existing := p.discoverVM(ctx, vm.vmName); existing != nil && existing.vmID != "" {
		p.removeVM(ctx, existing.vmID)
	}

	// Clone VM from snapshot.
	args := buildCloneArgs(vm.vmName, cpu, mem, storage, cloneImage)
	out, err := p.cocoonExec(ctx, args...)
	if err != nil {
		logger.Errorf(ctx, err, "%s: clone failed: %s", key, out)
		return
	}

	vmID := parseVMID(out)
	// Discover new VM.
	var fresh *CocoonVM
	for range 5 {
		if vmID != "" {
			fresh = p.discoverVMByID(ctx, vmID)
		}
		if fresh == nil {
			fresh = p.discoverVM(ctx, vm.vmName)
		}
		if fresh != nil && fresh.vmID != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if fresh == nil {
		logger.Warnf(ctx, "%s: VM not found after clone", key)
		return
	}

	// Wait for DHCP IP.
	fresh.podNamespace = pod.Namespace
	fresh.podName = pod.Name
	fresh.os = spec.osType
	fresh.managed = true
	fresh.ip = p.waitForDHCPIP(ctx, fresh, 120*time.Second)

	// Update VM record.
	p.mu.Lock()
	vm.vmID = fresh.vmID
	vm.state = stateRunning
	vm.ip = fresh.ip
	vm.mac = fresh.mac
	vm.startedAt = time.Now()
	p.mu.Unlock()

	// Update in-memory pod annotations.
	p.syncPodRuntimeMetadata(ctx, key, fresh)

	// Post-boot inject + probes.
	go p.postBootInject(ctx, pod, vm)
	go p.startProbes(ctx, pod, vm)

	logger.Infof(ctx, "%s: complete (ip=%s)", key, fresh.ip)
	go p.notifyPodStatus(ctx, pod.Namespace, pod.Name)
}
