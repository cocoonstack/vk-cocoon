// Package provider — hibernate/wake lifecycle for pods.
//
// Hibernate: snapshot running VM → push to epoch → destroy VM → pod stays Running+NotReady.
// Wake: pull snapshot from epoch → clone VM → pod becomes Running+Ready.
//
// Triggered by the cocoon.cis/hibernate annotation:
//
//	annotation set to "true"  → hibernateVM (via reconcile loop)
//	annotation removed        → wakeVM     (via reconcile loop)
//
// The pod object stays alive in K8s throughout the hibernate/wake cycle,
// so Deployment/StatefulSet controllers don't recreate it.
package provider

import (
	"context"
	"os/exec"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// reconcileHibernateAnnotations checks all tracked pods for hibernate annotation
// changes. Called from the 30s reconcile loop because the VK framework doesn't
// trigger UpdatePod for annotation-only changes.
func (p *CocoonProvider) reconcileHibernateAnnotations(ctx context.Context) {
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
			klog.Infof("reconcileHibernate: %s has hibernate=true, triggering", c.key)
			p.hibernateVM(ctx, c.pod, c.vm)
		} else if liveHibernate != valTrue && c.vm.state == stateHibernated {
			klog.Infof("reconcileHibernate: %s hibernate removed, triggering wake", c.key)
			p.wakeVM(ctx, c.pod, c.vm)
		}
	}
}

// hibernateVM snapshots a running VM, pushes to epoch, and destroys the VM.
// The pod stays alive with phase=Running, Ready=False (container Waiting "Hibernated").
func (p *CocoonProvider) hibernateVM(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	key := podKey(pod.Namespace, pod.Name)

	// Slot-0 is the main agent — cannot be hibernated.
	if isMainAgent(vm.vmName) {
		klog.Warningf("hibernateVM %s: REJECTED — slot-0 (main agent) cannot be hibernated", key)
		return
	}

	klog.Infof("hibernateVM %s: starting (vm=%s id=%s)", key, vm.vmName, vm.vmID)

	// Stop probes — VM will be gone.
	p.stopProbes(key)

	// Resolve epoch registry URL.
	imageRaw := ann(pod, AnnImage, "")
	if imageRaw == "" && len(pod.Spec.Containers) > 0 {
		imageRaw = pod.Spec.Containers[0].Image
	}
	registryURL, _ := parseImageRef(imageRaw)
	puller := p.getPuller(registryURL)

	// 1. Snapshot the running VM.
	snapshotName := vm.vmName + "-suspend"
	_, _ = p.cocoonExec(ctx, "snapshot", "rm", snapshotName)

	out, err := p.cocoonExec(ctx, "snapshot", "save", "--name", snapshotName, vm.vmID)
	if err != nil {
		klog.Errorf("hibernateVM %s: snapshot failed: %v — %s", key, err, out)
		return
	}
	klog.Infof("hibernateVM %s: snapshot %s created", key, snapshotName)

	// 2. Push to epoch.
	pushedToEpoch := false
	if puller != nil {
		_ = exec.CommandContext(ctx, "sudo", "chmod", "-R", "a+rX", //nolint:gosec // trusted path from config
			filepath.Join(puller.RootDir(), "snapshot", "localfile")).Run()

		if pushErr := puller.PushSnapshot(ctx, snapshotName, "latest"); pushErr != nil {
			klog.Errorf("hibernateVM %s: epoch push failed: %v", key, pushErr)
		} else {
			klog.Infof("hibernateVM %s: pushed to epoch", key)
			pushedToEpoch = true
		}
	}

	// Record snapshot ref for wake.
	fullRef := snapshotName
	if registryURL != "" && pushedToEpoch {
		fullRef = registryURL + "/" + snapshotName
	}
	p.recordSuspendedSnapshot(ctx, pod, vm.vmName, fullRef)

	if pushedToEpoch {
		_, _ = p.cocoonExec(ctx, "snapshot", "rm", snapshotName)
	}

	// 3. Destroy VM.
	_, _ = p.cocoonExec(ctx, "vm", "rm", "--force", vm.vmID)

	// 4. Mark as hibernated (pod stays, VM gone).
	p.mu.Lock()
	vm.state = stateHibernated
	vm.vmID = ""
	vm.ip = ""
	p.mu.Unlock()

	klog.Infof("hibernateVM %s: complete — pod stays alive, VM destroyed", key)
	go p.notifyPodStatus(pod.Namespace, pod.Name)
}

// wakeVM restores a hibernated VM from its epoch snapshot.
// Triggered when the cocoon.cis/hibernate annotation is removed.
func (p *CocoonProvider) wakeVM(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	key := podKey(pod.Namespace, pod.Name)
	klog.Infof("wakeVM %s: starting (vm=%s)", key, vm.vmName)

	// Resolve image and check for suspended snapshot.
	imageRaw := ann(pod, AnnImage, "")
	if imageRaw == "" && len(pod.Spec.Containers) > 0 {
		imageRaw = pod.Spec.Containers[0].Image
	}
	registryURL, image := parseImageRef(imageRaw)
	cloneImage := image

	if ref := p.lookupSuspendedSnapshot(ctx, pod.Namespace, vm.vmName); ref != "" {
		klog.Infof("wakeVM %s: found suspended snapshot %s", key, ref)
		suspendRegistry, suspendName := parseImageRef(ref)
		cloneImage = suspendName
		if suspendRegistry != "" {
			registryURL = suspendRegistry
		}
		p.clearSuspendedSnapshot(ctx, pod.Namespace, vm.vmName)
	}

	// Pull from epoch.
	puller := p.getPuller(registryURL)
	if puller != nil {
		if err := puller.EnsureSnapshot(ctx, cloneImage); err != nil {
			klog.Warningf("wakeVM %s: epoch pull %s failed: %v", key, cloneImage, err)
		}
	}

	// Resource limits.
	cpu, mem := podResourceLimits(pod)
	storage := ann(pod, AnnStorage, "100G")

	// Clean up any stale VM with same name.
	if existing := p.discoverVM(ctx, vm.vmName); existing != nil && existing.vmID != "" {
		_, _ = p.cocoonExec(ctx, "vm", "rm", "--force", existing.vmID)
	}

	// Clone VM from snapshot.
	args := buildCloneArgs(vm.vmName, cpu, mem, storage, cloneImage)
	out, err := p.cocoonExec(ctx, args...)
	if err != nil {
		klog.Errorf("wakeVM %s: clone failed: %v — %s", key, err, out)
		return
	}

	// Discover new VM.
	var fresh *CocoonVM
	for range 5 {
		fresh = p.discoverVM(ctx, vm.vmName)
		if fresh != nil && fresh.vmID != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if fresh == nil {
		klog.Errorf("wakeVM %s: VM not found after clone", key)
		return
	}

	// Wait for DHCP IP.
	fresh.podNamespace = pod.Namespace
	fresh.podName = pod.Name
	fresh.os = ann(pod, AnnOS, "linux")
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
	go p.startProbes(context.Background(), pod, vm)

	klog.Infof("wakeVM %s: complete (ip=%s)", key, fresh.ip)
	go p.notifyPodStatus(pod.Namespace, pod.Name)
}
