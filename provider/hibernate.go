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
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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
		if vm == nil || !vm.Managed {
			continue
		}
		checks = append(checks, check{key: key, pod: pod, vm: vm})
	}
	p.mu.RUnlock()

	for _, c := range checks {
		// Read the LIVE annotation from K8s (p.pods may be stale).
		cmd := fmt.Sprintf(
			`kubectl get pod %s -n %s -o jsonpath='{.metadata.annotations.cocoon\.cis/hibernate}' 2>/dev/null`,
			c.pod.Name, c.pod.Namespace)
		out, _ := exec.CommandContext(ctx, "bash", "-c", cmd).Output()
		liveHibernate := strings.Trim(strings.TrimSpace(string(out)), "'")

		if liveHibernate == "true" && c.vm.State == "running" {
			klog.Infof("reconcileHibernate: %s has hibernate=true, triggering", c.key)
			p.hibernateVM(ctx, c.pod, c.vm)
		} else if liveHibernate != "true" && c.vm.State == "hibernated" {
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
	if isMainAgent(vm.VMName) {
		klog.Warningf("hibernateVM %s: REJECTED — slot-0 (main agent) cannot be hibernated", key)
		return
	}

	klog.Infof("hibernateVM %s: starting (vm=%s id=%s)", key, vm.VMName, vm.VMID)

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
	snapshotName := vm.VMName + "-suspend"
	p.cocoonExec(ctx, "snapshot", "rm", snapshotName)

	out, err := p.cocoonExec(ctx, "snapshot", "save", "--name", snapshotName, vm.VMID)
	if err != nil {
		klog.Errorf("hibernateVM %s: snapshot failed: %v — %s", key, err, out)
		return
	}
	klog.Infof("hibernateVM %s: snapshot %s created", key, snapshotName)

	// 2. Push to epoch.
	pushedToEpoch := false
	if puller != nil {
		_ = exec.CommandContext(ctx, "sudo", "chmod", "-R", "a+rX",
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
	p.recordSuspendedSnapshot(ctx, pod, vm.VMName, fullRef)

	if pushedToEpoch {
		p.cocoonExec(ctx, "snapshot", "rm", snapshotName)
	}

	// 3. Destroy VM.
	p.cocoonExec(ctx, "vm", "rm", "--force", vm.VMID)

	// 4. Mark as hibernated (pod stays, VM gone).
	p.mu.Lock()
	vm.State = "hibernated"
	vm.VMID = ""
	vm.IP = ""
	p.mu.Unlock()

	klog.Infof("hibernateVM %s: complete — pod stays alive, VM destroyed", key)
	go p.notifyPodStatus(pod.Namespace, pod.Name)
}

// wakeVM restores a hibernated VM from its epoch snapshot.
// Triggered when the cocoon.cis/hibernate annotation is removed.
func (p *CocoonProvider) wakeVM(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	key := podKey(pod.Namespace, pod.Name)
	klog.Infof("wakeVM %s: starting (vm=%s)", key, vm.VMName)

	// Resolve image and check for suspended snapshot.
	imageRaw := ann(pod, AnnImage, "")
	if imageRaw == "" && len(pod.Spec.Containers) > 0 {
		imageRaw = pod.Spec.Containers[0].Image
	}
	registryURL, image := parseImageRef(imageRaw)
	cloneImage := image

	if ref := p.lookupSuspendedSnapshot(ctx, pod.Namespace, vm.VMName); ref != "" {
		klog.Infof("wakeVM %s: found suspended snapshot %s", key, ref)
		suspendRegistry, suspendName := parseImageRef(ref)
		cloneImage = suspendName
		if suspendRegistry != "" {
			registryURL = suspendRegistry
		}
		p.clearSuspendedSnapshot(ctx, pod.Namespace, vm.VMName)
	}

	// Pull from epoch.
	puller := p.getPuller(registryURL)
	if puller != nil {
		if err := puller.EnsureSnapshot(ctx, cloneImage); err != nil {
			klog.Warningf("wakeVM %s: epoch pull %s failed: %v", key, cloneImage, err)
		}
	}

	// Resource limits.
	cpu := "2"
	mem := "8G"
	if c := pod.Spec.Containers; len(c) > 0 {
		if q := c[0].Resources.Limits.Cpu(); q != nil && !q.IsZero() {
			cpu = fmt.Sprintf("%d", q.Value())
		}
		if q := c[0].Resources.Limits.Memory(); q != nil && !q.IsZero() {
			mb := q.Value() / (1024 * 1024)
			if mb >= 1024 {
				mem = fmt.Sprintf("%dG", mb/1024)
			} else {
				mem = fmt.Sprintf("%dM", mb)
			}
		}
	}
	storage := ann(pod, AnnStorage, "100G")

	// Clean up any stale VM with same name.
	if existing := p.discoverVM(ctx, vm.VMName); existing != nil && existing.VMID != "" {
		p.cocoonExec(ctx, "vm", "rm", "--force", existing.VMID)
	}

	// Clone VM from snapshot.
	args := buildCloneArgs(vm.VMName, cpu, mem, storage, cloneImage)
	out, err := p.cocoonExec(ctx, args...)
	if err != nil {
		klog.Errorf("wakeVM %s: clone failed: %v — %s", key, err, out)
		return
	}

	// Discover new VM.
	var fresh *CocoonVM
	for i := 0; i < 5; i++ {
		fresh = p.discoverVM(ctx, vm.VMName)
		if fresh != nil && fresh.VMID != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if fresh == nil {
		klog.Errorf("wakeVM %s: VM not found after clone", key)
		return
	}

	// Wait for DHCP IP.
	fresh.PodNamespace = pod.Namespace
	fresh.PodName = pod.Name
	fresh.OS = ann(pod, AnnOS, "linux")
	fresh.Managed = true
	fresh.IP = p.waitForDHCPIP(ctx, fresh, 120*time.Second)

	// Update VM record.
	p.mu.Lock()
	vm.VMID = fresh.VMID
	vm.State = "running"
	vm.IP = fresh.IP
	vm.MAC = fresh.MAC
	vm.StartedAt = time.Now()
	p.mu.Unlock()

	// Update in-memory pod annotations.
	p.syncPodRuntimeMetadata(ctx, key, fresh)

	// Post-boot inject + probes.
	go p.postBootInject(ctx, pod, vm)
	go p.startProbes(context.Background(), pod, vm)

	klog.Infof("wakeVM %s: complete (ip=%s)", key, fresh.IP)
	go p.notifyPodStatus(pod.Namespace, pod.Name)
}
