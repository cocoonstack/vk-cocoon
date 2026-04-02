// Package provider implements a Virtual Kubelet provider that maps
// Kubernetes Pods to Cocoon MicroVMs.
//
// VM lifecycle (snapshot-based):
//
//	CreatePod  → derive stable VM name (with slot for Deployments)
//	           → check ConfigMap for suspended snapshot → pull from epoch
//	           → cocoon vm clone --cold → VM running
//	DeletePod  → detect scale-down vs restart:
//	           → restart/kill: snapshot save → push epoch → record → destroy VM
//	           → scale-down:   clear snapshot record → destroy VM (no snapshot)
//	           → replicas=0:   snapshot all (suspend group)
//
// Annotation reference (set on Pod):
//
//	cocoon.cis/image          — snapshot or image name (default: container image field)
//	cocoon.cis/mode           — "clone" (from snapshot) or "run" (from image). Default: clone
//	cocoon.cis/storage        — COW disk size (e.g. "100G"). Default: "10G"
//	cocoon.cis/nics           — NIC count (default: "1")
//	cocoon.cis/static-ip      — static IP for bridge-static network
//	cocoon.cis/dns            — comma-separated DNS servers
//	cocoon.cis/root-password  — root password for cloudimg VMs
//	cocoon.cis/ssh-password   — SSH password (optional default via COCOON_SSH_PASSWORD)
//	cocoon.cis/managed        — "true" if VK should snapshot+destroy VM on pod delete. Auto-set for VK-created VMs.
//	cocoon.cis/os             — "linux" (default) or "windows". Windows VMs use RDP (3389) instead of SSH.
//	cocoon.cis/vm-id          — (status) Cocoon VM ID
//	cocoon.cis/ip             — (status) VM IP address
//	cocoon.cis/mac            — (status) VM MAC address
//	cocoon.cis/snapshot-from  — (status) source snapshot used for clone
package provider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/projecteru2/core/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

const (
	// Annotation keys
	AnnImage        = meta.AnnotationImage
	AnnMode         = meta.AnnotationMode // clone | run
	AnnStorage      = meta.AnnotationStorage
	AnnNICs         = "cocoon.cis/nics"
	AnnStaticIP     = "cocoon.cis/static-ip"
	AnnDNS          = "cocoon.cis/dns"
	AnnRootPassword = "cocoon.cis/root-password" //nolint:gosec // annotation key, not a credential
	AnnSSHPassword  = "cocoon.cis/ssh-password"  //nolint:gosec // annotation key, not a credential
	AnnManaged      = meta.AnnotationManaged
	AnnOS           = meta.AnnotationOS // linux | windows
	AnnVMName       = meta.AnnotationVMName
	AnnVMID         = meta.AnnotationVMID
	AnnIP           = meta.AnnotationIP
	AnnMAC          = "cocoon.cis/mac"
	AnnSnapshotFrom = "cocoon.cis/snapshot-from"
	AnnHibernate    = meta.AnnotationHibernate // "true" → hibernate VM (pod stays)

	// CocoonSet controller annotations
	AnnForkFrom       = meta.AnnotationForkFrom       // VM name to fork from (set by CocoonSet controller)
	AnnSnapshotPolicy = meta.AnnotationSnapshotPolicy // always | main-only | never (set by CocoonSet controller)

	// VM state constants
	stateRunning    = "running"
	stateStopped    = "stopped"
	stateHibernated = "hibernated"
	valTrue         = "true"
	osWindows       = "windows"
)

// CocoonVM is the internal representation of a Cocoon VM tracked by the provider.
type CocoonVM struct {
	podNamespace string
	podName      string
	vmID         string
	vmName       string
	ip           string
	mac          string
	state        string // running, stopped, created, error
	cpu          int
	memoryMB     int
	image        string
	os           string // "linux" or "windows"
	managed      bool   // true = VK owns lifecycle (stop on delete, start on recreate)
	createdAt    time.Time
	startedAt    time.Time
}

// cocoonVMJSON matches `cocoon vm list --format json` output.
type cocoonVMJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	State   string `json:"state"`
	CPU     int    `json:"cpu"`
	Memory  int64  `json:"memory"`  // bytes
	Storage int64  `json:"storage"` // bytes
	IP      string `json:"ip"`
	Image   string `json:"image"`
	Created string `json:"created"`
	Config  struct {
		Name    string `json:"name"`
		CPU     int    `json:"cpu"`
		Memory  int64  `json:"memory"`
		Storage int64  `json:"storage"`
		Image   string `json:"image"`
		NICs    int    `json:"nics"`
	} `json:"config"`
	NetworkConfigs []struct {
		MAC     string `json:"mac"`
		Network struct {
			IP string `json:"ip"`
		} `json:"network"`
	} `json:"network_configs"`
}

// CocoonProvider implements nodeutil.Provider and node.PodNotifier.
type CocoonProvider struct {
	cocoonBin   string
	nodeIP      string
	kubeClient  kubernetes.Interface
	sshPassword string
	mu          sync.RWMutex
	pods        map[string]*corev1.Pod
	vms         map[string]*CocoonVM

	// PodNotifier callback — set by NotifyPods, called on status changes.
	notifyPodCb func(*corev1.Pod)

	// Epoch pullers keyed by registry URL (created on demand from image field).
	pullers map[string]*EpochPuller

	// K8s listers for ConfigMap/Secret volume injection.
	configMapLister corev1listers.ConfigMapLister
	secretLister    corev1listers.SecretLister

	// Content hashes for change detection (env + volumes).
	injectHashes map[string]string // podKey+"/env" or podKey+"/vol" -> sha256

	// Probe state per pod.
	probeStates map[string]*probeResult

	// Test hooks.
	discoverVMFn              func(context.Context, string) *CocoonVM
	discoverVMByIDFn          func(context.Context, string) *CocoonVM
	lookupSuspendedSnapshotFn func(context.Context, string, string) string
	cocoonExecFn              func(context.Context, ...string) (string, error)
}

var _ nodeutil.Provider = (*CocoonProvider)(nil)

// NewCocoonProvider creates a CocoonProvider that maps Kubernetes pods to Cocoon MicroVMs.
func NewCocoonProvider(ctx context.Context, cocoonBin, nodeIP string, kubeClient kubernetes.Interface, cfg nodeutil.ProviderConfig) *CocoonProvider {
	p := &CocoonProvider{
		cocoonBin:       cocoonBin,
		nodeIP:          nodeIP,
		kubeClient:      kubeClient,
		sshPassword:     os.Getenv("COCOON_SSH_PASSWORD"),
		pods:            make(map[string]*corev1.Pod),
		vms:             make(map[string]*CocoonVM),
		configMapLister: cfg.ConfigMaps,
		secretLister:    cfg.Secrets,
		injectHashes:    make(map[string]string),
		probeStates:     make(map[string]*probeResult),
		pullers:         make(map[string]*EpochPuller),
	}

	log.WithFunc("provider.NewCocoonProvider").Infof(ctx, "initialized (bin=%s, nodeIP=%s)", cocoonBin, nodeIP)

	// Start reconciliation loop (like kubelet's syncPod).
	go p.reconcileLoop(ctx)

	return p
}

// runConfig bundles the parameters for buildRunArgs.
type runConfig struct {
	vmName  string
	cpu     string
	mem     string
	storage string
	nics    string
	dns     string
	rootPwd string
	image   string
	osType  string
}

func buildRunArgs(rc runConfig) []string {
	args := []string{"vm", "run", "--name", rc.vmName, "--cpu", rc.cpu, "--memory", rc.mem, "--storage", rc.storage, "--nics", rc.nics}
	if isWindowsOS(rc.osType) {
		args = append(args, "--windows")
	}
	if rc.dns != "" {
		args = append(args, "--dns", rc.dns)
	}
	// Windows guests do not consume Cocoon's cloud-init root password path.
	if rc.rootPwd != "" && !isWindowsOS(rc.osType) {
		args = append(args, "--default-root-password", rc.rootPwd)
	}
	return append(args, rc.image)
}

func buildCloneArgs(vmName, cpu, mem, storage, snapshot string) []string {
	return []string{"vm", "clone", "--cold", "--name", vmName, "--cpu", cpu, "--memory", mem, "--storage", storage, snapshot}
}

// ---------- PodLifecycleHandler ----------

func (p *CocoonProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error { //nolint:gocyclo // pod creation orchestrates multiple subsystems
	key := podKey(pod.Namespace, pod.Name)
	logger := log.WithFunc("provider.CreatePod")
	logger.Infof(ctx, "%s", key)

	mode := ann(pod, AnnMode, modeClone)
	spec := resolvePodSpec(pod)
	runImage := spec.runImage()
	registryURL, image := spec.registryURL, spec.cloneImage()
	storage := spec.storage
	nics := spec.nics
	dns := spec.dns
	rootPwd := spec.rootPwd
	osType := spec.osType

	// "static" mode: track an externally-managed VM by IP. No cocoon interaction.
	if mode == modeStatic {
		vmIP := ann(pod, AnnIP, "")
		if vmIP == "" {
			return fmt.Errorf("static mode requires cocoon.cis/ip annotation")
		}
		vm := &CocoonVM{
			podNamespace: pod.Namespace,
			podName:      pod.Name,
			vmName:       pod.Name,
			vmID:         ann(pod, AnnVMID, "static-"+pod.Name),
			ip:           vmIP,
			state:        stateRunning,
			os:           spec.osType,
			managed:      false,
			cpu:          2,
			memoryMB:     4096,
			createdAt:    time.Now(),
			startedAt:    time.Now(),
		}
		p.storePodVM(ctx, key, pod, vm)
		logger.Infof(ctx, "%s: static VM ip=%s os=%s", key, vm.ip, vm.os)
		go p.notifyPodStatus(ctx, pod.Namespace, pod.Name)
		return nil
	}

	// "adopt" mode: attach to an existing Cocoon VM by name, don't create anything.
	if mode == modeAdopt { //nolint:nestif // adopt mode has required validation steps
		adoptName := ann(pod, AnnVMName, "")
		adoptID := ann(pod, AnnVMID, "")
		var vm *CocoonVM
		if adoptID != "" {
			vm = p.discoverVMByID(ctx, adoptID)
		} else if adoptName != "" {
			vm = p.discoverVM(ctx, adoptName)
		}
		if vm == nil {
			return fmt.Errorf("adopt: VM not found (name=%q id=%q)", adoptName, adoptID)
		}
		vm.podNamespace = pod.Namespace
		vm.podName = pod.Name
		vm.managed = ann(pod, AnnManaged, "false") == valTrue
		vm.os = spec.osType

		if ipAnn := ann(pod, AnnIP, ""); ipAnn != "" {
			vm.ip = ipAnn
		}
		if vm.ip == "" {
			vm.ip = p.resolveIPFromLease(vm.vmName)
		}

		p.storePodVM(ctx, key, pod, vm)
		logger.Infof(ctx, "%s: adopted existing VM %s (%s) ip=%s", key, vm.vmName, vm.vmID, vm.ip)
		return nil
	}

	if shouldRecoverManagedPod(mode, pod) && p.recoverManagedPod(ctx, pod, key, image, osType) {
		return nil
	}

	// ── Derive stable VM name + reserve slot atomically ──
	// deriveStableVMName → allocateSlot reads p.vms. We must hold the
	// write lock through both the allocation and the reservation to
	// prevent concurrent CreatePods from getting the same slot.
	p.mu.Lock()
	vmName := p.deriveStableVMNameLocked(ctx, pod)
	p.vms[key] = &CocoonVM{vmName: vmName, state: stateCreating, managed: true}
	p.mu.Unlock()

	// ── Determine clone source ──
	// Priority: 1. suspended snapshot (restore) → 2. fork from slot-0 (sub-agent) → 3. base image (new main)
	cloneImage := image
	slot := extractSlotFromVMName(vmName)
	snapshots := p.snapshotManager()

	if mode == modeClone { //nolint:nestif // clone source resolution has multiple fallback paths
		if ref := snapshots.lookupSuspendedSnapshot(ctx, pod.Namespace, vmName); ref != "" {
			// Restore from previously saved snapshot.
			logger.Infof(ctx, "%s: restoring from suspended snapshot %s", key, ref)
			suspendRegistry, suspendName := parseImageRef(ref)
			cloneImage = suspendName
			if suspendRegistry != "" {
				registryURL = suspendRegistry
			}
			// Don't clear for non-0 agents — their snapshots are permanent.
			if slot == 0 {
				snapshots.clearSuspendedSnapshot(ctx, pod.Namespace, vmName)
			}
		} else if forkSource := ann(pod, AnnForkFrom, ""); forkSource != "" {
			// CocoonSet controller specified fork source
			if forkSnap := p.forkFromVM(ctx, pod.Namespace, forkSource, vmName); forkSnap != "" {
				cloneImage = forkSnap
				logger.Infof(ctx, "%s: forking from %s (CocoonSet annotation), snapshot %s", key, forkSource, forkSnap)
			}
		} else if slot > 0 {
			// Legacy Deployment path: fork from the main agent (slot-0) by live-snapshotting it.
			if forkSnap := p.forkFromMainAgent(ctx, pod.Namespace, vmName); forkSnap != "" {
				cloneImage = forkSnap
				logger.Infof(ctx, "%s: forking sub-agent from slot-0 snapshot %s", key, forkSnap)
			}
		}
	}

	// Pre-check: if a VM with this name already exists (e.g. provider restart
	// or stale from a previous run), clean it up first.
	if existing := p.discoverVM(ctx, vmName); existing != nil && existing.vmID != "" {
		logger.Infof(ctx, "%s: stale VM %s exists (%s), removing", key, vmName, existing.state)
		_, _ = p.cocoonExec(ctx, "vm", "rm", "--force", existing.vmID)
	}

	// Resource limits → cocoon flags
	cpu, mem := podResourceLimits(pod)

	// Auto-pull snapshot from epoch registry if not available locally.
	puller := p.getPuller(ctx, registryURL)
	effectiveMode := mode
	effectiveCloneImage := cloneImage
	if puller != nil { //nolint:nestif // epoch pull logic with mode-specific handling
		switch mode {
		case modeClone:
			if err := puller.EnsureSnapshot(ctx, cloneImage); err != nil {
				logger.Warnf(ctx, "%s: epoch pull %s failed (will try local): %v", key, cloneImage, err)
			}
		case modeRun:
			if osType == osWindows {
				// Windows epoch refs currently point to direct qcow2 manifests.
				// Import them into the local cloudimg store and keep run mode.
				if err := puller.EnsureCloudImage(ctx, image); err != nil {
					logger.Warnf(ctx, "%s: epoch cloud image import %s failed (will try direct run): %v", key, image, err)
				} else {
					runImage = image
				}
			} else {
				// Linux epoch URLs represent snapshot repositories, not direct qcow2/OCI artifacts.
				// Pull the snapshot first, then cold-clone it locally even if the workload asked for run.
				if err := puller.EnsureSnapshot(ctx, image); err != nil {
					logger.Warnf(ctx, "%s: epoch pull %s failed (will try direct run): %v", key, image, err)
				} else {
					effectiveMode = modeClone
					effectiveCloneImage = image
				}
			}
		}
	}

	var args []string
	switch effectiveMode {
	case modeRun:
		args = buildRunArgs(runConfig{
			vmName:  vmName,
			cpu:     cpu,
			mem:     mem,
			storage: storage,
			nics:    nics,
			dns:     dns,
			rootPwd: rootPwd,
			image:   runImage,
			osType:  osType,
		})
	default: // clone
		args = buildCloneArgs(vmName, cpu, mem, storage, effectiveCloneImage)
	}

	out, err := p.cocoonExec(ctx, args...)
	if err != nil {
		logger.Errorf(ctx, err, "%s: %s", key, out)
		// Release the slot reservation on failure.
		p.mu.Lock()
		delete(p.vms, key)
		p.mu.Unlock()
		return fmt.Errorf("cocoon %s: %w", effectiveMode, err)
	}
	logImage := effectiveCloneImage
	if effectiveMode == modeRun {
		logImage = runImage
	}
	logger.Infof(ctx, "%s: cocoon %s OK (requested=%s image=%s)", key, effectiveMode, mode, logImage)

	// Discover VM details (retry — cold boot may take a moment to register)
	var vm *CocoonVM
	for range 5 {
		vm = p.discoverVM(ctx, vmName)
		if vm != nil && vm.vmID != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if vm == nil {
		vm = &CocoonVM{vmName: vmName, state: stateRunning, cpu: 2, memoryMB: 8192}
	}
	logger.Infof(ctx, "%s: discovered VM %s (vmid=%s)", key, vmName, vm.vmID)
	vm.podNamespace = pod.Namespace
	vm.podName = pod.Name
	vm.image = image
	if mode == modeRun {
		vm.image = runImage
	}
	vm.os = osType
	vm.managed = true
	vm.createdAt = time.Now()
	vm.startedAt = time.Now()

	// Wait for DHCP IP. Snapshots use DHCP networking (10.88.100.x).
	vm.ip = p.waitForDHCPIP(ctx, vm, 120*time.Second)

	// Update pod annotations with VM info
	snapshotFrom := cloneImage
	if mode == modeRun {
		snapshotFrom = runImage
	}
	p.storePodVM(ctx, key, pod, vm, podAnnotation{key: AnnSnapshotFrom, value: snapshotFrom})

	// Post-boot: inject env vars + ConfigMap/Secret volumes via SSH.
	go p.postBootInject(ctx, pod, vm)

	// Start liveness/readiness probes if defined.
	go p.startProbes(ctx, pod, vm)

	// Async notify pod status to avoid 5s polling delay.
	go p.notifyPodStatus(ctx, pod.Namespace, pod.Name)
	return nil
}

func (p *CocoonProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	key := podKey(pod.Namespace, pod.Name)

	// Detect hibernate/wake transition BEFORE updating the store.
	p.mu.RLock()
	oldPod := p.pods[key]
	vm := p.vms[key]
	p.mu.RUnlock()

	wasHibernated := oldPod != nil && ann(oldPod, AnnHibernate, "") == valTrue
	wantHibernate := ann(pod, AnnHibernate, "") == valTrue

	// Update pod in store (preserve our status annotations).
	p.mu.Lock()
	if existing, ok := p.pods[key]; ok {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		for _, k := range []string{AnnVMName, AnnVMID, AnnIP, AnnMAC, AnnManaged, AnnSnapshotFrom} {
			if v, ok := existing.Annotations[k]; ok {
				pod.Annotations[k] = v
			}
		}
	}
	p.pods[key] = pod.DeepCopy()
	p.mu.Unlock()

	// Trigger hibernate/wake on annotation change.
	if vm != nil {
		if !wasHibernated && wantHibernate && vm.state == stateRunning {
			go p.hibernateVM(ctx, pod, vm)
			return nil
		}
		if wasHibernated && !wantHibernate && vm.state == stateHibernated {
			go p.wakeVM(ctx, pod, vm)
			return nil
		}
	}

	// Re-inject env/volumes on update (detects ConfigMap/Secret changes).
	p.mu.RLock()
	vm = p.vms[key]
	p.mu.RUnlock()
	if vm != nil && vm.os != osWindows && vm.ip != "" && vm.state == stateRunning {
		go p.postBootInject(ctx, pod, vm)
	}
	return nil
}

func (p *CocoonProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	key := podKey(pod.Namespace, pod.Name)
	logger := log.WithFunc("provider.DeletePod")
	logger.Infof(ctx, "%s", key)

	p.mu.RLock()
	vm, ok := p.vms[key]
	p.mu.RUnlock()
	snapshots := p.snapshotManager()

	switch {
	case ok && vm.vmID != "" && vm.managed:
		if ownerKey := p.findOtherActivePodForVMID(ctx, pod, vm.vmID); ownerKey != "" {
			logger.Warnf(ctx, "%s: skip destroy for VM %s (%s); still owned by active pod %s", key, vm.vmName, vm.vmID, ownerKey)
			goto cleanup
		}
		if p.shouldSnapshotOnDelete(ctx, pod) { //nolint:nestif // snapshot-on-delete has epoch push pipeline
			// Pod restart / kill / suspend: snapshot → push to epoch → record.
			// Mark VM as suspending immediately so allocateSlot in a concurrent
			// CreatePod (the replacement) can reuse this slot.
			p.mu.Lock()
			vm.state = stateSuspending
			p.mu.Unlock()

			spec := resolvePodSpec(pod)
			puller := p.getPuller(ctx, spec.registryURL)

			snapshotName := vm.vmName + "-suspend"
			logger.Infof(ctx, "%s: creating snapshot %s from running VM %s", key, snapshotName, vm.vmID)

			out, err := snapshots.saveSnapshot(ctx, snapshotName, vm.vmID)
			if err != nil {
				logger.Errorf(ctx, err, "%s: snapshot failed: %s", key, out)
			} else {
				logger.Infof(ctx, "%s: snapshot %s created", key, snapshotName)

				// Push snapshot to epoch (if registry configured).
				pushedToEpoch := false
				if puller != nil {
					_ = exec.CommandContext(ctx, "sudo", "chmod", "-R", "a+rX", //nolint:gosec // trusted path from config
						filepath.Join(puller.RootDir(), "snapshot", "localfile")).Run()

					if pushErr := puller.PushSnapshot(ctx, snapshotName, "latest"); pushErr != nil {
						logger.Errorf(ctx, pushErr, "%s: epoch push failed", key)
					} else {
						logger.Infof(ctx, "%s: snapshot pushed to epoch", key)
						pushedToEpoch = true
					}
				}

				// Record snapshot ref for next CreatePod.
				fullRef := snapshotName
				if spec.registryURL != "" && pushedToEpoch {
					fullRef = spec.registryURL + "/" + snapshotName
				}
				snapshots.recordSuspendedSnapshot(ctx, pod, vm.vmName, fullRef)

				// Only clean up local snapshot if safely in epoch.
				if pushedToEpoch {
					snapshots.removeSnapshot(ctx, snapshotName)
				}
			}
		} else {
			// Scale-down: skip snapshot.
			logger.Infof(ctx, "%s: scale-down detected, skipping snapshot", key)
			// Only clear snapshot for slot-0 (main agent). Sub-agent snapshots
			// are permanent — they can be restored via Hibernation CRD.
			if isMainAgent(vm.vmName) {
				snapshots.clearSuspendedSnapshot(ctx, pod.Namespace, vm.vmName)
			}
		}

		// Destroy VM (always, regardless of snapshot decision).
		logger.Infof(ctx, "%s: destroying VM %s (%s)", key, vm.vmName, vm.vmID)
		_, _ = p.cocoonExec(ctx, "vm", "rm", "--force", vm.vmID)

	case ok && !vm.managed:
		logger.Infof(ctx, "%s: skipping unmanaged VM %s (%s)", key, vm.vmName, vm.vmID)
	case !ok:
		// Fallback: provider lost in-memory state (restart etc).
		vmID := ann(pod, AnnVMID, "")
		managed := ann(pod, AnnManaged, "")
		if vmID != "" && managed == valTrue {
			if ownerKey := p.findOtherActivePodForVMID(ctx, pod, vmID); ownerKey != "" {
				logger.Warnf(ctx, "%s: skip fallback destroy for vm-id=%s; still owned by active pod %s", key, vmID, ownerKey)
				goto cleanup
			}
			logger.Infof(ctx, "%s: fallback destroy via annotation vm-id=%s", key, vmID)
			_, _ = p.cocoonExec(ctx, "vm", "rm", "--force", vmID)
		}
	}

	// Stop probes, clean up injection hashes, remove DNS entry.
cleanup:
	p.stopProbes(key)
	removePodDNS(pod.Name)
	p.mu.Lock()
	delete(p.pods, key)
	delete(p.vms, key)
	delete(p.injectHashes, key+"/env")
	delete(p.injectHashes, key+"/vol")
	p.mu.Unlock()
	return nil
}
