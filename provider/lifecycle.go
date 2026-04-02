// Package provider — extended pod lifecycle: init containers, multi-container,
// security context, DownwardAPI, events, resource enforcement.
//
// VM semantics:
//   - Init containers: SSH commands run sequentially before main service
//   - Multi-container: each container -> systemd service in the VM
//   - Security context: runAsUser -> Linux user inside VM
//   - Container restart: systemd Restart=always; track count from journalctl
//   - Resource enforcement: CH API balloon/vcpu resize
//   - Pod events: k8s EventRecorder for lifecycle events
package provider

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/cocoonstack/cocoon-operator/cocoonmeta"
	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------- #13: Init Containers ----------

// runInitContainers executes init containers sequentially via SSH.
// Each init container's command is run inside the VM. If any fails,
// the pod is marked as failed. Called before starting main containers.
func (p *CocoonProvider) runInitContainers(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) error {
	if vm.skipSSH() {
		return nil
	}
	logger := log.WithFunc("provider.runInitContainers")
	pw := p.sshPass(vm)
	for i, ic := range pod.Spec.InitContainers {
		cmd := strings.Join(append(ic.Command, ic.Args...), " ")
		if cmd == "" {
			continue
		}
		logger.Infof(ctx, "initContainer[%d] %s/%s: running %q", i, pod.Namespace, pod.Name, cmd)
		out, err := p.guestExecutor().execSimple(ctx, vm, pw, cmd)
		if err != nil {
			logger.Errorf(ctx, err, "initContainer[%d] %s/%s failed: %s", i, pod.Namespace, pod.Name, out)
			return fmt.Errorf("init container %s failed: %w", ic.Name, err)
		}
		logger.Infof(ctx, "initContainer[%d] %s/%s: OK (%d bytes output)", i, pod.Namespace, pod.Name, len(out))
	}
	return nil
}

// ---------- #12: Multi-container -> systemd services ----------

// installContainerServices creates a systemd service for each container spec.
// Container[0] is the primary; additional containers are sidecar services.
// Each service gets its own env file and command.
func (p *CocoonProvider) installContainerServices(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	if vm.skipSSH() || len(pod.Spec.Containers) <= 1 {
		return // single container handled by deploy.sh / standard flow
	}
	logger := log.WithFunc("provider.installContainerServices")
	pw := p.sshPass(vm)
	for i, c := range pod.Spec.Containers {
		if i == 0 {
			continue // primary container managed by existing flow
		}
		svcName := fmt.Sprintf("sidecar-%s", c.Name)
		cmd := strings.Join(append(c.Command, c.Args...), " ")
		if cmd == "" {
			continue
		}

		// Write env file for this container
		envContent := ""
		for _, e := range c.Env {
			if e.Value != "" {
				envContent += fmt.Sprintf("%s=%s\n", e.Name, e.Value)
			}
		}
		if envContent != "" {
			envPath := fmt.Sprintf("/opt/agent/sidecar-%s.env", c.Name)
			_ = p.guestExecutor().writeFile(ctx, vm, pw, envPath, []byte(envContent), 0o600)
		}

		// Create systemd service
		unit := fmt.Sprintf(`[Unit]
Description=Sidecar: %s
After=network.target

[Service]
Type=simple
ExecStart=%s
EnvironmentFile=-/opt/agent/sidecar-%s.env
Restart=always
RestartSec=10
StandardOutput=append:/opt/agent/logs/%s.log
StandardError=append:/opt/agent/logs/%s.log

[Install]
WantedBy=multi-user.target
`, c.Name, cmd, c.Name, svcName, svcName)

		unitPath := fmt.Sprintf("/etc/systemd/system/%s.service", svcName)
		_ = p.guestExecutor().writeFile(ctx, vm, pw, unitPath, []byte(unit), 0o644)
		_, _ = p.guestExecutor().execSimple(ctx, vm, pw, fmt.Sprintf("systemctl daemon-reload && systemctl enable %s && systemctl start %s", svcName, svcName))
		logger.Infof(ctx, "%s/%s: sidecar %s started", pod.Namespace, pod.Name, svcName)
	}
}

// ---------- #14: Security Context ----------

// applySecurityContext maps pod/container security settings to VM config.
// For VMs, the main mapping is runAsUser -> create/switch Linux user.
// Capabilities, seccomp, apparmor are N/A (VM provides kernel-level isolation).
func (p *CocoonProvider) applySecurityContext(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	if vm.skipSSH() {
		return
	}
	logger := log.WithFunc("provider.applySecurityContext")
	sc := pod.Spec.SecurityContext
	if sc == nil && (len(pod.Spec.Containers) == 0 || pod.Spec.Containers[0].SecurityContext == nil) {
		return
	}

	pw := p.sshPass(vm)

	// Pod-level runAsUser
	var uid *int64
	if sc != nil && sc.RunAsUser != nil {
		uid = sc.RunAsUser
	}
	// Container-level overrides pod-level
	if len(pod.Spec.Containers) > 0 {
		csc := pod.Spec.Containers[0].SecurityContext
		if csc != nil && csc.RunAsUser != nil {
			uid = csc.RunAsUser
		}
	}

	if uid != nil && *uid != 0 {
		// Ensure user exists
		username := fmt.Sprintf("app-%d", *uid)
		cmd := fmt.Sprintf("id -u %d >/dev/null 2>&1 || useradd -u %d -m %s", *uid, *uid, username)
		_, _ = p.guestExecutor().execSimple(ctx, vm, pw, cmd)
		logger.Infof(ctx, "%s/%s: runAsUser=%d (user=%s)",
			pod.Namespace, pod.Name, *uid, username)
	}

	// ReadOnlyRootFilesystem: log warning only (VM needs writable root to function)
	if len(pod.Spec.Containers) > 0 {
		csc := pod.Spec.Containers[0].SecurityContext
		if csc != nil && csc.ReadOnlyRootFilesystem != nil && *csc.ReadOnlyRootFilesystem {
			logger.Warnf(ctx, "%s/%s: ReadOnlyRootFilesystem ignored (VM requires writable root)",
				pod.Namespace, pod.Name)
		}
	}
}

// ---------- #22: DownwardAPI as env vars ----------

// injectDownwardAPIEnv adds pod metadata to the env file.
// These are the standard k8s downward API fields.
func (p *CocoonProvider) injectDownwardAPIEnv(pod *corev1.Pod, vm *CocoonVM) map[string]string {
	env := map[string]string{
		"POD_NAME":      pod.Name,
		"POD_NAMESPACE": pod.Namespace,
		"POD_IP":        vm.ip,
		"NODE_NAME":     pod.Spec.NodeName,
		"POD_UID":       string(pod.UID),
	}
	if sa := pod.Spec.ServiceAccountName; sa != "" {
		env["SERVICE_ACCOUNT_NAME"] = sa
	}
	// Pod labels as POD_LABEL_xxx
	for k, v := range pod.Labels {
		key := "POD_LABEL_" + strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(k, "/", "_"), ".", "_"))
		env[key] = v
	}
	return env
}

// ---------- #24: Resource Enforcement via CH API ----------

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

	// CPU resize via CH API Unix socket.
	// Uses sudo curl because the API socket is root-owned; a Go-native
	// Unix socket client would require running the provider as root.
	if cpuQ := limits.Cpu(); cpuQ != nil && !cpuQ.IsZero() { //nolint:nestif // CH API resize with validation
		desiredCPU := int(cpuQ.Value())
		if desiredCPU > 0 && desiredCPU != vm.cpu {
			sock := chSocketPath(vm.vmID)
			if sock != "" {
				body := fmt.Sprintf(`{"desired_vcpus":%d}`, desiredCPU)
				cmd := fmt.Sprintf("sudo curl -s -X PUT --unix-socket %s -H 'Content-Type: application/json' -d '%s' http://localhost/api/v1/vm.resize",
					sock, body)
				if out, err := exec.CommandContext(ctx, "bash", "-c", cmd).CombinedOutput(); err != nil { //nolint:gosec // cmd from trusted internal template
					logger.Debugf(ctx, "%s: CPU resize to %d failed: %v (%s)",
						vm.vmName, desiredCPU, err, strings.TrimSpace(string(out)))
				} else {
					logger.Infof(ctx, "%s: CPU resized to %d", vm.vmName, desiredCPU)
					vm.cpu = desiredCPU
				}
			}
		}
	}

	// Memory enforcement via balloon (CH doesn't support memory hotplug down).
	// We could set balloon to reclaim excess memory, but this is complex and
	// risky for running VMs. Log the desired size for observability.
	if memQ := limits.Memory(); memQ != nil && !memQ.IsZero() {
		desiredMB := int(memQ.Value() / (1024 * 1024))
		if desiredMB > 0 && desiredMB != vm.memoryMB {
			logger.Debugf(ctx, "%s: memory limit %dMB (VM configured %dMB, balloon resize not applied)",
				vm.vmName, desiredMB, vm.memoryMB)
		}
	}
}

// ---------- #16: SSH Key Authentication ----------

// injectSSHKey writes an SSH public key to the VM's authorized_keys.
// If annotation cocoon.cis/ssh-pubkey is set, inject it.
func (p *CocoonProvider) injectSSHKey(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	if vm.skipSSH() {
		return
	}
	logger := log.WithFunc("provider.injectSSHKey")
	pubkey := ann(pod, "cocoon.cis/ssh-pubkey", "")
	if pubkey == "" {
		return
	}
	pw := p.sshPass(vm)
	cmd := fmt.Sprintf("mkdir -p /root/.ssh && echo '%s' >> /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys",
		pubkey)
	if _, err := p.guestExecutor().execSimple(ctx, vm, pw, cmd); err != nil {
		logger.Warnf(ctx, "%s/%s: %v", pod.Namespace, pod.Name, err)
	} else {
		logger.Infof(ctx, "%s/%s: SSH pubkey injected", pod.Namespace, pod.Name)
	}
}

// ---------- #17: Pod DNS via dnsmasq ----------

// addPodDNS adds a dnsmasq host entry for the pod: pod-name -> VM IP.
// Pods can resolve each other by name within the cocoon bridge.
func addPodDNS(podName, namespace, ip string) {
	if ip == "" {
		return
	}
	// Add to /etc/hosts for local resolution
	entry := fmt.Sprintf("%s\t%s %s.%s.svc.cluster.local", ip, podName, podName, namespace)
	// Append if not already present
	cmd := fmt.Sprintf("grep -q '%s' /etc/hosts 2>/dev/null || echo '%s' >> /etc/hosts", podName, entry)
	_, _ = exec.Command("sudo", "bash", "-c", cmd).CombinedOutput() //nolint:gosec // cmd from trusted template
}

// removePodDNS removes the dnsmasq host entry for the pod.
func removePodDNS(podName string) {
	cmd := fmt.Sprintf("sudo sed -i '/%s/d' /etc/hosts 2>/dev/null", podName)
	_ = exec.Command("bash", "-c", cmd).Run() //nolint:gosec
}

// ---------- Owner Detection ----------

// getOwnerDeploymentName returns the Deployment name for a pod owned via
// ReplicaSet, or "" if not a Deployment-owned pod.
func (p *CocoonProvider) getOwnerDeploymentName(ctx context.Context, pod *corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "ReplicaSet" { //nolint:nestif
			rsName := ref.Name
			if idx := strings.LastIndex(rsName, "-"); idx > 0 {
				candidate := rsName[:idx]
				rs, err := p.kubeClient.AppsV1().ReplicaSets(pod.Namespace).Get(ctx, rsName, metav1.GetOptions{})
				if err == nil {
					for _, ownerRef := range rs.OwnerReferences {
						if ownerRef.Kind == "Deployment" {
							return candidate
						}
					}
				}
			}
			break
		}
	}
	return ""
}

// getOwnerStatefulSetName returns the StatefulSet name if the pod is owned by one.
func getOwnerStatefulSetName(pod *corev1.Pod) string {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "StatefulSet" {
			return ref.Name
		}
	}
	return ""
}

// extractOrdinal extracts the ordinal index from a StatefulSet pod name.
// e.g. "agent-group-2" -> 2, "bot-0" -> 0.
func extractOrdinal(podName string) int {
	if idx := strings.LastIndex(podName, "-"); idx >= 0 {
		if n, err := strconv.Atoi(podName[idx+1:]); err == nil {
			return n
		}
	}
	return -1
}

// ---------- Scale Detection ----------

// getDesiredReplicas queries the desired replica count for a Deployment or StatefulSet.
// Returns -1 if the resource is not found (deleted) or the query fails.
func (p *CocoonProvider) getDesiredReplicas(ctx context.Context, ns, kind, name string) int {
	if kind == "StatefulSet" {
		sts, err := p.kubeClient.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return -1
		}
		if sts.Spec.Replicas == nil {
			return -1
		}
		return int(*sts.Spec.Replicas)
	}
	deploy, err := p.kubeClient.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return -1
	}
	if deploy.Spec.Replicas == nil {
		return -1
	}
	return int(*deploy.Spec.Replicas)
}

// shouldSnapshotOnDelete determines whether a pod's VM should be snapshotted
// and pushed to epoch before destruction. Returns false for scale-down (the
// replica slot is being permanently removed), true for pod restart / kill /
// deployment suspend (replicas=0).
//
// Decision matrix:
//
//	Deployment  desired=0              -> true  (suspend all, snapshot everything)
//	Deployment  desired>0, count>desired -> false (scale-down, slot removed)
//	Deployment  desired>0, count<=desired -> true  (pod restart / kill)
//	StatefulSet desired=0              -> true  (suspend all)
//	StatefulSet ordinal>=desired        -> false (scale-down, highest ordinals removed first)
//	StatefulSet ordinal<desired        -> true  (pod restart / kill)
//	Bare pod                           -> true  (always snapshot)
//	Owner not found (-1)               -> true  (conservative: assume restart)
func (p *CocoonProvider) shouldSnapshotOnDelete(ctx context.Context, pod *corev1.Pod) bool {
	logger := log.WithFunc("provider.shouldSnapshotOnDelete")
	key := podKey(pod.Namespace, pod.Name)

	// CocoonSet-owned pods: controller sets explicit snapshot policy
	if policy := ann(pod, AnnSnapshotPolicy, ""); policy != "" {
		switch policy {
		case "never":
			logger.Infof(ctx, "%s: annotation snapshot-policy=never", key)
			return false
		case "always":
			logger.Infof(ctx, "%s: annotation snapshot-policy=always", key)
			return true
		case "main-only":
			vm := p.getVM(pod.Namespace, pod.Name)
			if vm != nil && isMainAgent(vm.vmName) {
				logger.Infof(ctx, "%s: annotation snapshot-policy=main-only (is main)", key)
				return true
			}
			logger.Infof(ctx, "%s: annotation snapshot-policy=main-only (not main, skip)", key)
			return false
		}
	}

	// Explicit hibernate annotation from operator — always snapshot.
	if ann(pod, AnnHibernate, "") == valTrue {
		logger.Infof(ctx, "%s: hibernate annotation set, snapshot", key)
		return true
	}

	// StatefulSet: scale-down always removes highest ordinal first.
	if stsName := getOwnerStatefulSetName(pod); stsName != "" {
		desired := p.getDesiredReplicas(ctx, pod.Namespace, "StatefulSet", stsName)
		if desired <= 0 {
			return true // suspend all, or owner gone — be conservative
		}
		ordinal := extractOrdinal(pod.Name)
		if ordinal >= 0 && ordinal >= desired {
			logger.Infof(ctx, "%s: StatefulSet scale-down (ordinal=%d >= desired=%d), skip",
				key, ordinal, desired)
			return false
		}
		return true
	}

	// Deployment: compare tracked pod count with desired replicas.
	if deployName := p.getOwnerDeploymentName(ctx, pod); deployName != "" {
		desired := p.getDesiredReplicas(ctx, pod.Namespace, "Deployment", deployName)
		if desired <= 0 {
			return true // suspend all, or owner gone
		}
		current := p.countDeploymentPods(pod.Namespace, deployName)
		if current > desired {
			logger.Infof(ctx, "%s: Deployment scale-down (current=%d > desired=%d), skip",
				key, current, desired)
			return false
		}
		return true
	}

	// Bare pod: always snapshot.
	return true
}

// countDeploymentPods counts pods tracked by the provider that belong to the
// given Deployment (matched by ReplicaSet owner ref prefix).
func (p *CocoonProvider) countDeploymentPods(ns, deployName string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	for _, pod := range p.pods {
		if pod.Namespace != ns {
			continue
		}
		for _, ref := range pod.OwnerReferences {
			if ref.Kind == "ReplicaSet" {
				if idx := strings.LastIndex(ref.Name, "-"); idx > 0 {
					if ref.Name[:idx] == deployName {
						count++
					}
				}
			}
		}
	}
	return count
}

// ---------- Slot Allocation ----------

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

// ---------- Stable VM Name Derivation ----------

// deriveStableVMNameLocked resolves a stable VM name. Caller must hold p.mu (write lock).
// This ensures slot allocation + reservation is atomic.
func (p *CocoonProvider) deriveStableVMNameLocked(ctx context.Context, pod *corev1.Pod) string {
	if name := ann(pod, AnnVMName, ""); name != "" {
		return name
	}

	if deployName := p.getOwnerDeploymentName(ctx, pod); deployName != "" {
		slot := p.allocateSlotLocked(pod.Namespace, deployName)
		return cocoonmeta.VMNameForDeployment(pod.Namespace, deployName, slot)
	}

	return cocoonmeta.VMNameForPod(pod.Namespace, pod.Name)
}

// ---------- Fork / Slot Helpers ----------

// extractSlotFromVMName extracts the slot number from a Deployment VM name.
// "vk-prod-deploy-2" -> 2.  Returns -1 for non-Deployment names.
func extractSlotFromVMName(vmName string) int {
	return cocoonmeta.ExtractSlotFromVMName(vmName)
}

// mainAgentVMName derives the slot-0 VM name from any slot's VM name.
// "vk-prod-deploy-2" -> "vk-prod-deploy-0"
func mainAgentVMName(vmName string) string {
	return cocoonmeta.MainAgentVMName(vmName)
}

// forkFromMainAgent creates a live snapshot of the main agent (slot-0) VM
// and returns the snapshot name for the sub-agent to clone from.
// Returns "" if the main agent is not running or snapshot fails.
func (p *CocoonProvider) forkFromMainAgent(ctx context.Context, _, vmName string) string {
	logger := log.WithFunc("provider.forkFromMainAgent")
	mainVM := mainAgentVMName(vmName)
	snapshots := p.snapshotManager()

	// Find the slot-0 VM.
	p.mu.RLock()
	var sourceVM *CocoonVM
	for _, vm := range p.vms {
		if vm.vmName == mainVM && vm.state == stateRunning && vm.vmID != "" {
			sourceVM = vm
			break
		}
	}
	p.mu.RUnlock()

	if sourceVM == nil {
		logger.Warnf(ctx, "main agent VM %s not running, falling back to base image", mainVM)
		return ""
	}

	// Live snapshot the main agent.
	forkSnap := vmName + "-fork"
	out, err := snapshots.saveSnapshot(ctx, forkSnap, sourceVM.vmID)
	if err != nil {
		logger.Errorf(ctx, err, "snapshot %s failed: %s", mainVM, out)
		return ""
	}
	logger.Infof(ctx, "live snapshot of %s (%s) -> %s", mainVM, sourceVM.vmID, forkSnap)
	return forkSnap
}

// forkFromVM creates a live snapshot of a specific source VM.
// Used by CocoonSet controller via cocoon.cis/fork-from annotation.
func (p *CocoonProvider) forkFromVM(ctx context.Context, _, sourceVMName, targetVMName string) string {
	logger := log.WithFunc("provider.forkFromVM")
	snapshots := p.snapshotManager()
	p.mu.RLock()
	var sourceVM *CocoonVM
	for _, vm := range p.vms {
		if vm.vmName == sourceVMName && vm.state == stateRunning && vm.vmID != "" {
			sourceVM = vm
			break
		}
	}
	p.mu.RUnlock()

	if sourceVM == nil {
		logger.Warnf(ctx, "source VM %s not running, falling back to base image", sourceVMName)
		return ""
	}

	forkSnap := targetVMName + "-fork"
	out, err := snapshots.saveSnapshot(ctx, forkSnap, sourceVM.vmID)
	if err != nil {
		logger.Errorf(ctx, err, "snapshot %s failed: %s", sourceVMName, out)
		return ""
	}
	logger.Infof(ctx, "live snapshot of %s (%s) -> %s", sourceVMName, sourceVM.vmID, forkSnap)
	return forkSnap
}

// isMainAgent returns true if the VM name represents slot-0 (the main agent).
func isMainAgent(vmName string) bool {
	return cocoonmeta.InferRoleFromVMName(vmName) == cocoonmeta.RoleMain
}
