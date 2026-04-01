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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

// Annotation keys.
const (
	AnnImage        = "cocoon.cis/image"
	AnnMode         = "cocoon.cis/mode" // clone | run
	AnnStorage      = "cocoon.cis/storage"
	AnnNICs         = "cocoon.cis/nics"
	AnnStaticIP     = "cocoon.cis/static-ip"
	AnnDNS          = "cocoon.cis/dns"
	AnnRootPassword = "cocoon.cis/root-password"
	AnnSSHPassword  = "cocoon.cis/ssh-password"
	AnnManaged      = "cocoon.cis/managed"
	AnnOS           = "cocoon.cis/os" // linux | windows
	AnnVMName       = "cocoon.cis/vm-name"
	AnnVMID         = "cocoon.cis/vm-id"
	AnnIP           = "cocoon.cis/ip"
	AnnMAC          = "cocoon.cis/mac"
	AnnSnapshotFrom = "cocoon.cis/snapshot-from"
	AnnHibernate    = "cocoon.cis/hibernate" // "true" → hibernate VM (pod stays)

	// CocoonSet controller annotations
	AnnForkFrom       = "cocoon.cis/fork-from"       // VM name to fork from (set by CocoonSet controller)
	AnnSnapshotPolicy = "cocoon.cis/snapshot-policy" // always | main-only | never (set by CocoonSet controller)
)

// CocoonVM is the internal representation of a Cocoon VM tracked by the provider.
type CocoonVM struct {
	PodNamespace string
	PodName      string
	VMID         string
	VMName       string
	IP           string
	MAC          string
	State        string // running, stopped, created, error
	CPU          int
	MemoryMB     int
	StorageGB    int
	NICs         int
	Image        string
	OS           string // "linux" or "windows"
	Managed      bool   // true = VK owns lifecycle (stop on delete, start on recreate)
	CreatedAt    time.Time
	StartedAt    time.Time
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

func NewCocoonProvider(cocoonBin, nodeIP string, kubeClient kubernetes.Interface, cfg nodeutil.ProviderConfig) *CocoonProvider {
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

	klog.Infof("CocoonProvider initialized (bin=%s, nodeIP=%s)", cocoonBin, nodeIP)

	// Start reconciliation loop (like kubelet's syncPod).
	go p.reconcileLoop()

	return p
}

func isWindowsOS(osType string) bool {
	return strings.EqualFold(strings.TrimSpace(osType), "windows")
}

func buildRunArgs(vmName, cpu, mem, storage, nics, dns, rootPwd, image, osType string) []string {
	args := []string{"vm", "run", "--name", vmName, "--cpu", cpu, "--memory", mem, "--storage", storage, "--nics", nics}
	if isWindowsOS(osType) {
		args = append(args, "--windows")
	}
	if dns != "" {
		args = append(args, "--dns", dns)
	}
	// Windows guests do not consume Cocoon's cloud-init root password path.
	if rootPwd != "" && !isWindowsOS(osType) {
		args = append(args, "--default-root-password", rootPwd)
	}
	return append(args, image)
}

func buildCloneArgs(vmName, cpu, mem, storage, snapshot string) []string {
	return []string{"vm", "clone", "--cold", "--name", vmName, "--cpu", cpu, "--memory", mem, "--storage", storage, snapshot}
}

func shouldRecoverManagedPod(mode string, pod *corev1.Pod) bool {
	if mode == "static" || mode == "adopt" {
		return false
	}
	return ann(pod, AnnVMID, "") != ""
}

func shouldReuseExistingVMState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "stopped", "stopped (stale)", "error", "failed":
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
	for attempt := 0; attempt < managedRecoveryAttempts; attempt++ {
		var vm *CocoonVM
		if vmID != "" {
			vm = p.discoverVMByID(ctx, vmID)
		}
		if vm == nil && vmName != "" {
			vm = p.discoverVM(ctx, vmName)
		}
		if vm != nil {
			last = vm
			if shouldReuseExistingVMState(vm.State) {
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

func (p *CocoonProvider) storeRecoveredPodVM(key string, pod *corev1.Pod, vm *CocoonVM) {
	changed := applyVMPodAnnotations(pod, vm)

	p.mu.Lock()
	p.pods[key] = pod.DeepCopy()
	p.vms[key] = vm
	p.mu.Unlock()

	p.patchPodAnnotations(context.Background(), pod.Namespace, pod.Name, changed)
}

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

func applyVMPodAnnotations(pod *corev1.Pod, vm *CocoonVM) map[string]string {
	changed := map[string]string{}
	setPodAnnotation(pod, changed, AnnVMName, vm.VMName)
	setPodAnnotation(pod, changed, AnnVMID, vm.VMID)
	setPodAnnotation(pod, changed, AnnIP, vm.IP)
	setPodAnnotation(pod, changed, AnnMAC, vm.MAC)
	if vm.Managed {
		setPodAnnotation(pod, changed, AnnManaged, "true")
	}
	return changed
}

func (p *CocoonProvider) patchPodAnnotations(ctx context.Context, ns, name string, annotations map[string]string) {
	if p.kubeClient == nil || len(annotations) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": annotations,
		},
	})
	if err != nil {
		klog.Warningf("patch pod annotations %s/%s: marshal patch: %v", ns, name, err)
		return
	}
	if _, err := p.kubeClient.CoreV1().Pods(ns).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		klog.Warningf("patch pod annotations %s/%s: %v", ns, name, err)
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
	if current, ok := p.vms[key]; ok {
		if vm.VMID != "" {
			current.VMID = vm.VMID
		}
		if vm.VMName != "" {
			current.VMName = vm.VMName
		}
		if vm.IP != "" {
			current.IP = vm.IP
		}
		if vm.MAC != "" {
			current.MAC = vm.MAC
		}
		if vm.State != "" {
			current.State = vm.State
		}
		if vm.Image != "" {
			current.Image = vm.Image
		}
		if vm.OS != "" {
			current.OS = vm.OS
		}
		if vm.Managed {
			current.Managed = true
		}
		if !vm.CreatedAt.IsZero() {
			current.CreatedAt = vm.CreatedAt
		}
		if !vm.StartedAt.IsZero() {
			current.StartedAt = vm.StartedAt
		}
		vm = current
	}
	if pod, ok := p.pods[key]; ok {
		changed = applyVMPodAnnotations(pod, vm)
		ns = pod.Namespace
		name = pod.Name
	}
	p.mu.Unlock()

	p.patchPodAnnotations(ctx, ns, name, changed)
}

func (p *CocoonProvider) recoverManagedPod(ctx context.Context, pod *corev1.Pod, key, image, osType string) bool {
	vmID := ann(pod, AnnVMID, "")
	vmName := ann(pod, AnnVMName, "")

	vm := p.discoverRecoverableManagedVM(ctx, vmID, vmName)

	if vm != nil && shouldReuseExistingVMState(vm.State) {
		if vm.VMID == "" {
			vm.VMID = vmID
		}
		if vm.VMName == "" {
			vm.VMName = vmName
		}
		if vm.IP == "" {
			if ipAnn := ann(pod, AnnIP, ""); ipAnn != "" {
				vm.IP = ipAnn
			}
		}
		if vm.IP == "" && vm.MAC != "" {
			if dhcpIP := resolveIPFromLeaseByMAC(vm.MAC); dhcpIP != "" {
				vm.IP = dhcpIP
			}
		}
		if vm.IP == "" && vm.VMName != "" {
			vm.IP = p.resolveIPFromLease(vm.VMName)
		}
		if vm.Image == "" {
			vm.Image = image
		}
		vm.PodNamespace = pod.Namespace
		vm.PodName = pod.Name
		vm.OS = osType
		vm.Managed = true
		now := time.Now()
		if vm.CreatedAt.IsZero() {
			vm.CreatedAt = now
		}
		if vm.StartedAt.IsZero() {
			vm.StartedAt = now
		}

		p.storeRecoveredPodVM(key, pod, vm)
		klog.Infof("CreatePod %s: recovered existing VM %s (%s) state=%s ip=%s", key, vm.VMName, vm.VMID, vm.State, vm.IP)
		go p.startProbes(context.Background(), pod, vm)
		go p.notifyPodStatus(pod.Namespace, pod.Name)
		return true
	}

	if ann(pod, AnnHibernate, "") == "true" && vmName != "" {
		if ref := p.lookupSuspendedSnapshot(ctx, pod.Namespace, vmName); ref != "" {
			now := time.Now()
			vm = &CocoonVM{
				PodNamespace: pod.Namespace,
				PodName:      pod.Name,
				VMName:       vmName,
				State:        "hibernated",
				Image:        image,
				OS:           osType,
				Managed:      true,
				CreatedAt:    now,
				StartedAt:    now,
			}
			p.storeRecoveredPodVM(key, pod, vm)
			klog.Infof("CreatePod %s: recovered hibernated pod %s from snapshot %s", key, vmName, ref)
			go p.notifyPodStatus(pod.Namespace, pod.Name)
			return true
		}
	}

	if vm != nil {
		klog.Warningf("CreatePod %s: pod carries existing vm-id=%s vm-name=%s but VM stayed in non-recoverable state=%s after retry; continuing with create", key, vmID, vmName, vm.State)
		return false
	}
	klog.Warningf("CreatePod %s: pod carries existing vm-id=%s vm-name=%s but no recoverable VM was found; continuing with create", key, vmID, vmName)
	return false
}

// reconcileLoop periodically checks all managed VMs and restarts any that
// are stopped or stale. Runs every 30 seconds, similar to kubelet's syncPod.
func (p *CocoonProvider) reconcileLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		p.reconcileOnce()
	}
}

func (p *CocoonProvider) reconcileOnce() {
	ctx := context.Background()

	// Check for hibernate/wake annotation changes (VK framework doesn't
	// call UpdatePod for annotation-only changes, so we poll here).
	p.reconcileHibernateAnnotations(ctx)

	// Snapshot the current managed VMs under the lock.
	p.mu.RLock()
	type vmSnapshot struct {
		key  string
		vmID string
		name string
	}
	var managed []vmSnapshot
	for key, vm := range p.vms {
		if vm.Managed {
			managed = append(managed, vmSnapshot{key: key, vmID: vm.VMID, name: vm.VMName})
		}
	}
	p.mu.RUnlock()

	if len(managed) == 0 {
		return
	}

	for _, snap := range managed {
		// Skip hibernated VMs — no running VM expected.
		p.mu.RLock()
		vmRec, ok := p.vms[snap.key]
		if ok && vmRec.State == "hibernated" {
			p.mu.RUnlock()
			continue
		}
		if !ok {
			p.mu.RUnlock()
			continue
		}
		updated := *vmRec
		p.mu.RUnlock()

		var fresh *CocoonVM
		if snap.vmID != "" {
			fresh = p.discoverVMByID(ctx, snap.vmID)
		}
		if fresh == nil && snap.name != "" {
			fresh = p.discoverVM(ctx, snap.name)
		}
		if fresh == nil {
			klog.Warningf("reconcile: VM %s (%s) not found by cocoon", snap.name, snap.vmID)
			continue
		}

		if snap.vmID == "" && fresh.VMID != "" {
			updated.VMID = fresh.VMID
		}
		if fresh.VMName != "" {
			updated.VMName = fresh.VMName
		}
		if fresh.State != "" {
			updated.State = fresh.State
		}
		if fresh.MAC != "" {
			updated.MAC = fresh.MAC
		}
		if fresh.IP != "" {
			updated.IP = fresh.IP
		}
		if updated.MAC != "" {
			if dhcpIP := resolveIPFromLeaseByMAC(updated.MAC); dhcpIP != "" {
				updated.IP = dhcpIP
			}
		}

		p.syncPodRuntimeMetadata(ctx, snap.key, &updated)

		if updated.State != "running" {
			klog.Warningf("reconcile: VM %s (%s) state=%s (not auto-restarting — VMs are ephemeral now)",
				snap.name, snap.vmID, updated.State)
		}
	}
}

func podKey(ns, name string) string { return ns + "/" + name }
func ann(pod *corev1.Pod, key, def string) string {
	if v, ok := pod.Annotations[key]; ok && v != "" {
		return v
	}
	return def
}

// parseImageRef splits an image reference into (registryURL, snapshotName).
// Supports:
//
//	"https://registry.example.com/ubuntu-dev-base" → ("https://registry.example.com", "ubuntu-dev-base")
//	"http://127.0.0.1:4300/ubuntu-dev-base"        → ("http://127.0.0.1:4300", "ubuntu-dev-base")
//	"ubuntu-dev-base"                              → ("", "ubuntu-dev-base")
func parseImageRef(image string) (registryURL, snapshotName string) {
	if strings.HasPrefix(image, "http://") || strings.HasPrefix(image, "https://") {
		// Find the last "/" that separates the registry URL from the snapshot name.
		// URL path could be just /name or /path/name.
		idx := strings.LastIndex(image, "/")
		if idx > 8 { // after "https://"
			return image[:idx], image[idx+1:]
		}
	}
	return "", image
}

// getPuller returns an EpochPuller for the given registry URL, creating one on demand.
func (p *CocoonProvider) getPuller(registryURL string) *EpochPuller {
	if registryURL == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ep, ok := p.pullers[registryURL]; ok {
		return ep
	}
	rootDir := os.Getenv("COCOON_ROOT")
	if rootDir == "" {
		rootDir = "/data01/cocoon"
	}
	ep := NewEpochPuller(registryURL, rootDir, p.cocoonBin)
	p.pullers[registryURL] = ep
	klog.Infof("Epoch puller created for %s (root=%s)", registryURL, rootDir)
	return ep
}

// ---------- PodLifecycleHandler ----------

func (p *CocoonProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	key := podKey(pod.Namespace, pod.Name)
	klog.Infof("CreatePod: %s", key)

	mode := ann(pod, AnnMode, "clone")

	// Resolve image: annotation > container image field.
	// Image can be a plain name ("ubuntu-dev-base") or an Epoch URL
	// ("https://registry.example.com/ubuntu-dev-base").
	imageRaw := ann(pod, AnnImage, "")
	if imageRaw == "" && len(pod.Spec.Containers) > 0 {
		imageRaw = pod.Spec.Containers[0].Image
	}
	if imageRaw == "" {
		imageRaw = "openclaw-agent-golden-v2"
	}
	registryURL, image := parseImageRef(imageRaw)
	runImage := imageRaw
	storage := ann(pod, AnnStorage, "100G")
	nics := ann(pod, AnnNICs, "1")
	dns := ann(pod, AnnDNS, "")
	rootPwd := ann(pod, AnnRootPassword, "")
	osType := ann(pod, AnnOS, "linux")

	// "static" mode: track an externally-managed VM by IP. No cocoon interaction.
	if mode == "static" {
		vmIP := ann(pod, AnnIP, "")
		if vmIP == "" {
			return fmt.Errorf("static mode requires cocoon.cis/ip annotation")
		}
		vm := &CocoonVM{
			PodNamespace: pod.Namespace,
			PodName:      pod.Name,
			VMName:       pod.Name,
			VMID:         ann(pod, AnnVMID, "static-"+pod.Name),
			IP:           vmIP,
			State:        "running",
			OS:           ann(pod, AnnOS, "linux"),
			Managed:      false,
			CPU:          2,
			MemoryMB:     4096,
			CreatedAt:    time.Now(),
			StartedAt:    time.Now(),
		}
		changed := applyVMPodAnnotations(pod, vm)
		p.mu.Lock()
		p.pods[key] = pod.DeepCopy()
		p.vms[key] = vm
		p.mu.Unlock()
		p.patchPodAnnotations(ctx, pod.Namespace, pod.Name, changed)
		klog.Infof("CreatePod %s: static VM ip=%s os=%s", key, vm.IP, vm.OS)
		go p.notifyPodStatus(pod.Namespace, pod.Name)
		return nil
	}

	// "adopt" mode: attach to an existing Cocoon VM by name, don't create anything.
	if mode == "adopt" {
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
		vm.PodNamespace = pod.Namespace
		vm.PodName = pod.Name
		vm.Managed = ann(pod, AnnManaged, "false") == "true"
		vm.OS = ann(pod, AnnOS, "linux")

		if ipAnn := ann(pod, AnnIP, ""); ipAnn != "" {
			vm.IP = ipAnn
		}
		if vm.IP == "" {
			vm.IP = p.resolveIPFromLease(vm.VMName)
		}

		changed := applyVMPodAnnotations(pod, vm)

		p.mu.Lock()
		p.pods[key] = pod.DeepCopy()
		p.vms[key] = vm
		p.mu.Unlock()
		p.patchPodAnnotations(ctx, pod.Namespace, pod.Name, changed)
		klog.Infof("CreatePod %s: adopted existing VM %s (%s) ip=%s", key, vm.VMName, vm.VMID, vm.IP)
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
	p.vms[key] = &CocoonVM{VMName: vmName, State: "creating", Managed: true}
	p.mu.Unlock()

	// ── Determine clone source ──
	// Priority: 1. suspended snapshot (restore) → 2. fork from slot-0 (sub-agent) → 3. base image (new main)
	cloneImage := image
	slot := extractSlotFromVMName(vmName)

	if mode == "clone" {
		if ref := p.lookupSuspendedSnapshot(ctx, pod.Namespace, vmName); ref != "" {
			// Restore from previously saved snapshot.
			klog.Infof("CreatePod %s: restoring from suspended snapshot %s", key, ref)
			suspendRegistry, suspendName := parseImageRef(ref)
			cloneImage = suspendName
			if suspendRegistry != "" {
				registryURL = suspendRegistry
			}
			// Don't clear for non-0 agents — their snapshots are permanent.
			if slot == 0 {
				p.clearSuspendedSnapshot(ctx, pod.Namespace, vmName)
			}
		} else if forkSource := ann(pod, AnnForkFrom, ""); forkSource != "" {
			// CocoonSet controller specified fork source
			if forkSnap := p.forkFromVM(ctx, pod.Namespace, forkSource, vmName); forkSnap != "" {
				cloneImage = forkSnap
				klog.Infof("CreatePod %s: forking from %s (CocoonSet annotation), snapshot %s", key, forkSource, forkSnap)
			}
		} else if slot > 0 {
			// Legacy Deployment path: fork from the main agent (slot-0) by live-snapshotting it.
			if forkSnap := p.forkFromMainAgent(ctx, pod.Namespace, vmName); forkSnap != "" {
				cloneImage = forkSnap
				klog.Infof("CreatePod %s: forking sub-agent from slot-0 snapshot %s", key, forkSnap)
			}
		}
	}

	// Pre-check: if a VM with this name already exists (e.g. provider restart
	// or stale from a previous run), clean it up first.
	if existing := p.discoverVM(ctx, vmName); existing != nil && existing.VMID != "" {
		klog.Infof("CreatePod %s: stale VM %s exists (%s), removing", key, vmName, existing.State)
		p.cocoonExec(ctx, "vm", "rm", "--force", existing.VMID)
	}

	// Resource limits → cocoon flags
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

	// Auto-pull snapshot from epoch registry if not available locally.
	puller := p.getPuller(registryURL)
	effectiveMode := mode
	effectiveCloneImage := cloneImage
	if puller != nil {
		switch mode {
		case "clone":
			if err := puller.EnsureSnapshot(ctx, cloneImage); err != nil {
				klog.Warningf("CreatePod %s: epoch pull %s failed (will try local): %v", key, cloneImage, err)
			}
		case "run":
			if osType == "windows" {
				// Windows epoch refs currently point to direct qcow2 manifests.
				// Import them into the local cloudimg store and keep run mode.
				if err := puller.EnsureCloudImage(ctx, image); err != nil {
					klog.Warningf("CreatePod %s: epoch cloud image import %s failed (will try direct run): %v", key, image, err)
				} else {
					runImage = image
				}
			} else {
				// Linux epoch URLs represent snapshot repositories, not direct qcow2/OCI artifacts.
				// Pull the snapshot first, then cold-clone it locally even if the workload asked for run.
				if err := puller.EnsureSnapshot(ctx, image); err != nil {
					klog.Warningf("CreatePod %s: epoch pull %s failed (will try direct run): %v", key, image, err)
				} else {
					effectiveMode = "clone"
					effectiveCloneImage = image
				}
			}
		}
	}

	var args []string
	switch effectiveMode {
	case "run":
		args = buildRunArgs(vmName, cpu, mem, storage, nics, dns, rootPwd, runImage, osType)
	default: // clone
		args = buildCloneArgs(vmName, cpu, mem, storage, effectiveCloneImage)
	}

	out, err := p.cocoonExec(ctx, args...)
	if err != nil {
		klog.Errorf("CreatePod %s: %v\n%s", key, err, out)
		// Release the slot reservation on failure.
		p.mu.Lock()
		delete(p.vms, key)
		p.mu.Unlock()
		return fmt.Errorf("cocoon %s: %w", effectiveMode, err)
	}
	logImage := effectiveCloneImage
	if effectiveMode == "run" {
		logImage = runImage
	}
	klog.Infof("CreatePod %s: cocoon %s OK (requested=%s image=%s)", key, effectiveMode, mode, logImage)

	// Discover VM details (retry — cold boot may take a moment to register)
	var vm *CocoonVM
	for i := 0; i < 5; i++ {
		vm = p.discoverVM(ctx, vmName)
		if vm != nil && vm.VMID != "" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if vm == nil {
		vm = &CocoonVM{VMName: vmName, State: "running", CPU: 2, MemoryMB: 8192}
	}
	klog.Infof("CreatePod %s: discovered VM %s (vmid=%s)", key, vmName, vm.VMID)
	vm.PodNamespace = pod.Namespace
	vm.PodName = pod.Name
	vm.Image = image
	if mode == "run" {
		vm.Image = runImage
	}
	vm.OS = osType
	vm.Managed = true
	vm.CreatedAt = time.Now()
	vm.StartedAt = time.Now()

	// Wait for DHCP IP. Snapshots use DHCP networking (10.88.100.x).
	vm.IP = p.waitForDHCPIP(ctx, vm, 120*time.Second)

	// Update pod annotations with VM info
	changed := applyVMPodAnnotations(pod, vm)
	setPodAnnotation(pod, changed, AnnSnapshotFrom, cloneImage)
	if mode == "run" {
		setPodAnnotation(pod, changed, AnnSnapshotFrom, runImage)
	}

	p.mu.Lock()
	p.pods[key] = pod.DeepCopy()
	p.vms[key] = vm
	p.mu.Unlock()
	p.patchPodAnnotations(ctx, pod.Namespace, pod.Name, changed)

	// Post-boot: inject env vars + ConfigMap/Secret volumes via SSH.
	go p.postBootInject(ctx, pod, vm)

	// Start liveness/readiness probes if defined.
	go p.startProbes(context.Background(), pod, vm)

	// Async notify pod status to avoid 5s polling delay.
	go p.notifyPodStatus(pod.Namespace, pod.Name)
	return nil
}

func (p *CocoonProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	key := podKey(pod.Namespace, pod.Name)

	// Detect hibernate/wake transition BEFORE updating the store.
	p.mu.RLock()
	oldPod := p.pods[key]
	vm := p.vms[key]
	p.mu.RUnlock()

	wasHibernated := oldPod != nil && ann(oldPod, AnnHibernate, "") == "true"
	wantHibernate := ann(pod, AnnHibernate, "") == "true"

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
		if !wasHibernated && wantHibernate && vm.State == "running" {
			go p.hibernateVM(ctx, pod, vm)
			return nil
		}
		if wasHibernated && !wantHibernate && vm.State == "hibernated" {
			go p.wakeVM(ctx, pod, vm)
			return nil
		}
	}

	// Re-inject env/volumes on update (detects ConfigMap/Secret changes).
	p.mu.RLock()
	vm = p.vms[key]
	p.mu.RUnlock()
	if vm != nil && vm.OS != "windows" && vm.IP != "" && vm.State == "running" {
		go p.postBootInject(ctx, pod, vm)
	}
	return nil
}

func (p *CocoonProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	key := podKey(pod.Namespace, pod.Name)
	klog.Infof("DeletePod: %s", key)

	p.mu.RLock()
	vm, ok := p.vms[key]
	p.mu.RUnlock()

	if ok && vm.VMID != "" && vm.Managed {
		if ownerKey := p.findOtherActivePodForVMID(ctx, pod, vm.VMID); ownerKey != "" {
			klog.Warningf("DeletePod %s: skip destroy for VM %s (%s); still owned by active pod %s", key, vm.VMName, vm.VMID, ownerKey)
			goto cleanup
		}
		if p.shouldSnapshotOnDelete(ctx, pod) {
			// Pod restart / kill / suspend: snapshot → push to epoch → record.
			// Mark VM as suspending immediately so allocateSlot in a concurrent
			// CreatePod (the replacement) can reuse this slot.
			p.mu.Lock()
			vm.State = "suspending"
			p.mu.Unlock()

			// Resolve the epoch registry URL from the pod's image annotation.
			imageRaw := ann(pod, AnnImage, "")
			if imageRaw == "" && len(pod.Spec.Containers) > 0 {
				imageRaw = pod.Spec.Containers[0].Image
			}
			delRegistryURL, _ := parseImageRef(imageRaw)
			puller := p.getPuller(delRegistryURL)

			snapshotName := vm.VMName + "-suspend"
			klog.Infof("DeletePod %s: creating snapshot %s from running VM %s", key, snapshotName, vm.VMID)

			// Remove old snapshot with same name first (cocoon rejects duplicate names).
			p.cocoonExec(ctx, "snapshot", "rm", snapshotName)

			out, err := p.cocoonExec(ctx, "snapshot", "save", "--name", snapshotName, vm.VMID)
			if err != nil {
				klog.Errorf("DeletePod %s: snapshot failed: %v — %s", key, err, out)
			} else {
				klog.Infof("DeletePod %s: snapshot %s created", key, snapshotName)

				// Push snapshot to epoch (if registry configured).
				pushedToEpoch := false
				if puller != nil {
					exec.CommandContext(ctx, "sudo", "chmod", "-R", "a+rX",
						filepath.Join(puller.RootDir(), "snapshot", "localfile")).Run()

					if pushErr := puller.PushSnapshot(ctx, snapshotName, "latest"); pushErr != nil {
						klog.Errorf("DeletePod %s: epoch push failed: %v", key, pushErr)
					} else {
						klog.Infof("DeletePod %s: snapshot pushed to epoch", key)
						pushedToEpoch = true
					}
				}

				// Record snapshot ref for next CreatePod.
				fullRef := snapshotName
				if delRegistryURL != "" && pushedToEpoch {
					fullRef = delRegistryURL + "/" + snapshotName
				}
				p.recordSuspendedSnapshot(ctx, pod, vm.VMName, fullRef)

				// Only clean up local snapshot if safely in epoch.
				if pushedToEpoch {
					p.cocoonExec(ctx, "snapshot", "rm", snapshotName)
				}
			}
		} else {
			// Scale-down: skip snapshot.
			klog.Infof("DeletePod %s: scale-down detected, skipping snapshot", key)
			// Only clear snapshot for slot-0 (main agent). Sub-agent snapshots
			// are permanent — they can be restored via Hibernation CRD.
			if isMainAgent(vm.VMName) {
				p.clearSuspendedSnapshot(ctx, pod.Namespace, vm.VMName)
			}
		}

		// Destroy VM (always, regardless of snapshot decision).
		klog.Infof("DeletePod %s: destroying VM %s (%s)", key, vm.VMName, vm.VMID)
		p.cocoonExec(ctx, "vm", "rm", "--force", vm.VMID)

	} else if ok && !vm.Managed {
		klog.Infof("DeletePod %s: skipping unmanaged VM %s (%s)", key, vm.VMName, vm.VMID)
	} else if !ok {
		// Fallback: provider lost in-memory state (restart etc).
		vmID := ann(pod, AnnVMID, "")
		managed := ann(pod, AnnManaged, "")
		if vmID != "" && managed == "true" {
			if ownerKey := p.findOtherActivePodForVMID(ctx, pod, vmID); ownerKey != "" {
				klog.Warningf("DeletePod %s: skip fallback destroy for vm-id=%s; still owned by active pod %s", key, vmID, ownerKey)
				goto cleanup
			}
			klog.Infof("DeletePod %s: fallback destroy via annotation vm-id=%s", key, vmID)
			p.cocoonExec(ctx, "vm", "rm", "--force", vmID)
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
		if key == selfKey || vm == nil || vm.VMID != vmID {
			continue
		}
		if otherPod, ok := p.pods[key]; ok {
			if otherPod.DeletionTimestamp != nil || isTerminalPodPhase(otherPod.Status.Phase) {
				continue
			}
			p.mu.RUnlock()
			return key
		}
		p.mu.RUnlock()
		return key
	}
	p.mu.RUnlock()

	if p.kubeClient == nil {
		return ""
	}

	list, err := p.kubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Warningf("findOtherActivePodForVMID %s: list pods: %v", vmID, err)
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

func (p *CocoonProvider) GetPod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pod, ok := p.pods[podKey(ns, name)]
	if !ok {
		return nil, fmt.Errorf("pod %s/%s not found", ns, name)
	}
	return pod, nil
}

func (p *CocoonProvider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
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

	// Refresh state from cocoon (preserve IP if inspect returns empty).
	// Skip for static VMs (externally managed, e.g. QEMU Windows VMs).
	if vm.VMID != "" && !strings.HasPrefix(vm.VMID, "static-") {
		if fresh := p.discoverVMByID(ctx, vm.VMID); fresh != nil {
			if fresh.VMID != "" {
				vm.VMID = fresh.VMID
			}
			if fresh.VMName != "" {
				vm.VMName = fresh.VMName
			}
			if fresh.State != "" {
				vm.State = fresh.State
			}
			if fresh.MAC != "" {
				vm.MAC = fresh.MAC
			}
			if fresh.IP != "" {
				vm.IP = fresh.IP
			}
		}
		// Always resolve DHCP IP — it's the actual accessible IP.
		// cocoon vm list returns CNI IP which differs from the guest DHCP IP,
		// and DHCP leases can change on VM restart/clone.
		dhcpIP := ""
		if vm.MAC != "" {
			dhcpIP = resolveIPFromLeaseByMAC(vm.MAC)
		}
		if dhcpIP == "" {
			dhcpIP = p.resolveIPFromLease(vm.VMName)
		}
		if dhcpIP != "" {
			vm.IP = dhcpIP
		}

		p.syncPodRuntimeMetadata(ctx, key, &vm)
	}

	phase := corev1.PodPending
	ready := corev1.ConditionFalse
	var containerState corev1.ContainerState

	switch vm.State {
	case "running":
		phase = corev1.PodRunning
		ready = corev1.ConditionTrue
		// Adjust readiness based on probe results.
		_, readinessOK := p.getProbeReadiness(key)
		if !readinessOK {
			ready = corev1.ConditionFalse
		}
		containerState = corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(vm.StartedAt)},
		}
	case "hibernated":
		// Pod stays Running (prevents RS/STS from recreating) but NotReady
		// (removes from Service endpoints). Container shows Waiting "Hibernated"
		// so the operator can detect completion.
		phase = corev1.PodRunning
		containerState = corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "Hibernated",
				Message: "VM suspended to epoch, waiting for wake",
			},
		}
	case "stopped":
		phase = corev1.PodSucceeded
		containerState = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Reason: "Stopped",
				StartedAt: metav1.NewTime(vm.StartedAt), FinishedAt: metav1.Now(),
			},
		}
	case "error":
		phase = corev1.PodFailed
		containerState = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Reason: "Error",
				StartedAt: metav1.NewTime(vm.StartedAt), FinishedAt: metav1.Now(),
			},
		}
	case "created":
		phase = corev1.PodPending
		containerState = corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "VMCreated"},
		}
	}

	containerName := "agent"
	if p.mu.RLock(); true {
		if pod, ok := p.pods[key]; ok && len(pod.Spec.Containers) > 0 {
			containerName = pod.Spec.Containers[0].Name
		}
		p.mu.RUnlock()
	}

	hostIP := p.nodeIP
	if hostIP == "" {
		hostIP = vm.IP
	}
	var podIPs []corev1.PodIP
	if vm.IP != "" {
		podIPs = []corev1.PodIP{{IP: vm.IP}}
	}

	return &corev1.PodStatus{
		Phase:  phase,
		HostIP: hostIP,
		PodIP:  vm.IP,
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
				Ready:        vm.State == "running",
				RestartCount: 0,
				Image:        vm.Image,
				ImageID:      vm.Image,
				ContainerID:  fmt.Sprintf("cocoon://%s", vm.VMID),
				State:        containerState,
			},
		},
	}, nil
}

// ---------- Logs / Exec / Attach / PortForward ----------

func (p *CocoonProvider) GetContainerLogs(ctx context.Context, ns, podName, container string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	klog.Infof("GetContainerLogs: %s/%s container=%s tail=%d follow=%v previous=%v timestamps=%v since=%d",
		ns, podName, container, opts.Tail, opts.Follow, opts.Previous, opts.Timestamps, opts.SinceSeconds)

	// Previous logs don't exist for VMs (no restart concept)
	if opts.Previous {
		return io.NopCloser(strings.NewReader("")), nil
	}

	vm := p.getVM(ns, podName)
	if vm == nil || vm.IP == "" {
		return io.NopCloser(strings.NewReader("pod not found or no IP assigned\n")), nil
	}

	// Windows VMs don't have SSH/journalctl — suggest RDP + Event Viewer
	if vm.OS == "windows" {
		msg := fmt.Sprintf("Windows VM %s (IP: %s) — use RDP (port 3389) for access.\n"+
			"  kubectl port-forward %s 3389:3389 -n %s\n"+
			"  Then connect with Remote Desktop to localhost:3389\n", vm.VMName, vm.IP, podName, ns)
		return io.NopCloser(strings.NewReader(msg)), nil
	}

	lines := "500"
	if opts.Tail > 0 {
		lines = strconv.Itoa(opts.Tail)
	}

	jcArgs := []string{"journalctl", "--no-pager", "-n", lines}
	if opts.Timestamps {
		jcArgs = append(jcArgs, "--output=short-iso")
	}
	if opts.Follow {
		jcArgs = append(jcArgs, "-f")
	}
	if opts.SinceSeconds > 0 {
		jcArgs = append(jcArgs, fmt.Sprintf("--since=%d seconds ago", opts.SinceSeconds))
	}
	if !opts.SinceTime.IsZero() {
		jcArgs = append(jcArgs, fmt.Sprintf("--since=%s", opts.SinceTime.Format("2006-01-02 15:04:05")))
	}

	args := []string{
		"-o", "StrictHostKeyChecking=no", "-o", "ConnectTimeout=5",
		"-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
		fmt.Sprintf("root@%s", vm.IP),
	}
	args = append(args, jcArgs...)

	fullArgs := append([]string{"-p", p.sshPass(vm), "ssh"}, args...)
	cmd := exec.CommandContext(ctx, "sshpass", fullArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return io.NopCloser(strings.NewReader(fmt.Sprintf("pipe error: %v\n", err))), nil
	}
	if err := cmd.Start(); err != nil {
		return io.NopCloser(strings.NewReader(fmt.Sprintf("ssh error: %v\n", err))), nil
	}
	return stdout, nil
}

func (p *CocoonProvider) RunInContainer(ctx context.Context, ns, podName, container string, cmd []string, attach api.AttachIO) error {
	vm := p.getVM(ns, podName)
	if vm == nil || vm.IP == "" {
		return fmt.Errorf("pod %s/%s not found or no IP", ns, podName)
	}

	klog.Infof("RunInContainer: %s/%s cmd=%v tty=%v", ns, podName, cmd, attach.TTY())

	// Windows VMs don't have SSH — return a helpful error
	if vm.OS == "windows" {
		msg := fmt.Sprintf("exec not supported on Windows VM. Use RDP:\n  kubectl port-forward %s 3389:3389 -n %s\n", podName, ns)
		if attach.Stdout() != nil {
			io.WriteString(attach.Stdout(), msg)
		}
		return fmt.Errorf("exec not supported on Windows VM (use RDP port 3389)")
	}

	sshArgs := []string{
		"-p", p.sshPass(vm), "ssh",
	}
	// Only allocate PTY for interactive sessions (kubectl exec -it).
	// Non-TTY commands (kubectl cp, kubectl exec without -t) send binary data
	// through stdin/stdout — PTY would corrupt it with terminal escaping.
	if attach.TTY() {
		sshArgs = append(sshArgs, "-tt")
	}
	sshArgs = append(sshArgs,
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
		fmt.Sprintf("root@%s", vm.IP),
	)
	// SSH remote command: join all args into one string for remote shell to parse.
	// Lens sends: ["sh", "-c", "(bash || ash || sh)"] as separate args.
	// SSH needs this as a single remote command string, not individual args
	// (otherwise each arg becomes a separate word in the remote shell).
	if len(cmd) > 0 {
		remoteCmd := shellQuoteJoin(cmd)
		sshArgs = append(sshArgs, "--", remoteCmd)
	}

	sshCmd := exec.CommandContext(ctx, "sshpass", sshArgs...)

	// Bridge SPDY streams ↔ SSH process via pipes
	stdinPipe, _ := sshCmd.StdinPipe()
	stdoutPipe, _ := sshCmd.StdoutPipe()
	stderrPipe, _ := sshCmd.StderrPipe()

	if err := sshCmd.Start(); err != nil {
		return fmt.Errorf("ssh start: %w", err)
	}

	done := make(chan struct{})

	// stdin: SPDY → SSH
	if attach.Stdin() != nil {
		go func() {
			io.Copy(stdinPipe, attach.Stdin())
			stdinPipe.Close()
		}()
	}
	// stdout: SSH → SPDY
	if attach.Stdout() != nil {
		go func() {
			io.Copy(attach.Stdout(), stdoutPipe)
			done <- struct{}{}
		}()
	}
	// stderr: SSH → SPDY
	if attach.Stderr() != nil {
		go func() {
			io.Copy(attach.Stderr(), stderrPipe)
		}()
	}
	// Terminal resize: forward window size changes to SSH via stty
	if attach.TTY() && attach.Resize() != nil {
		go func() {
			for size := range attach.Resize() {
				// Send stty command through stdin to resize terminal
				fmt.Fprintf(stdinPipe, "stty rows %d cols %d\n", size.Height, size.Width)
			}
		}()
	}

	// Wait for stdout to finish (main output channel)
	if attach.Stdout() != nil {
		<-done
	}
	return sshCmd.Wait()
}

func (p *CocoonProvider) AttachToContainer(ctx context.Context, ns, podName, container string, attach api.AttachIO) error {
	return p.RunInContainer(ctx, ns, podName, container, []string{"bash", "-l"}, attach)
}

func (p *CocoonProvider) PortForward(ctx context.Context, ns, podName string, port int32, stream io.ReadWriteCloser) error {
	vm := p.getVM(ns, podName)
	if vm == nil || vm.IP == "" {
		return fmt.Errorf("pod %s/%s not found or no IP", ns, podName)
	}

	// TCP proxy to VM IP:port
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", vm.IP, port), 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect to %s:%d: %w", vm.IP, port, err)
	}
	defer conn.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, stream); done <- struct{}{} }()
	go func() { io.Copy(stream, conn); done <- struct{}{} }()
	<-done
	return nil
}

// ---------- Stats / Metrics ----------

func (p *CocoonProvider) GetStatsSummary(ctx context.Context) (*statsv1alpha1.Summary, error) {
	now := metav1.Now()
	pods := make([]statsv1alpha1.PodStats, 0)

	p.mu.RLock()
	vmsCopy := make(map[string]*CocoonVM, len(p.vms))
	for k, v := range p.vms {
		vmsCopy[k] = v
	}
	p.mu.RUnlock()

	for key, vm := range vmsCopy {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) != 2 || vm.VMID == "" {
			continue
		}

		podStat := statsv1alpha1.PodStats{
			PodRef:    statsv1alpha1.PodReference{Name: parts[1], Namespace: parts[0]},
			StartTime: metav1.NewTime(vm.CreatedAt),
		}

		// Real CPU/MEM from CH API + /proc (skip static VMs)
		if !strings.HasPrefix(vm.VMID, "static-") {
			if ping, err := chGetPing(vm.VMID); err == nil && ping.PID > 0 {
				if uNs, sNs, err := readProcCPUUsage(ping.PID); err == nil {
					totalNs := uNs + sNs
					podStat.CPU = &statsv1alpha1.CPUStats{
						Time:                 now,
						UsageCoreNanoSeconds: &totalNs,
					}
				}
				if rss, err := readProcMemoryRSS(ping.PID); err == nil {
					podStat.Memory = &statsv1alpha1.MemoryStats{
						Time:            now,
						WorkingSetBytes: &rss,
					}
				}
			}
			// Network + Disk from vm.counters
			if counters, err := chGetCounters(vm.VMID); err == nil {
				for devName, stats := range counters {
					if strings.Contains(devName, "net") {
						rx := stats["rx_bytes"]
						tx := stats["tx_bytes"]
						podStat.Network = &statsv1alpha1.NetworkStats{
							Time: now,
							Interfaces: []statsv1alpha1.InterfaceStats{{
								Name:    devName,
								RxBytes: &rx,
								TxBytes: &tx,
							}},
						}
					}
				}
			}
		} else {
			// Static VMs: use configured values as fallback
			cpuNano := uint64(vm.CPU) * 1e9
			memBytes := uint64(vm.MemoryMB) * 1024 * 1024
			podStat.CPU = &statsv1alpha1.CPUStats{Time: now, UsageCoreNanoSeconds: &cpuNano}
			podStat.Memory = &statsv1alpha1.MemoryStats{Time: now, WorkingSetBytes: &memBytes}
		}
		pods = append(pods, podStat)
	}

	// Node stats from host /proc
	nodeCPU := uint64(0)
	nodeMem := readHostMemoryBytes()
	return &statsv1alpha1.Summary{
		Node: statsv1alpha1.NodeStats{
			NodeName:  "cocoon-pool",
			StartTime: now,
			CPU:       &statsv1alpha1.CPUStats{Time: now, UsageCoreNanoSeconds: &nodeCPU},
			Memory:    &statsv1alpha1.MemoryStats{Time: now, WorkingSetBytes: &nodeMem},
		},
		Pods: pods,
	}, nil
}

func (p *CocoonProvider) GetMetricsResource(ctx context.Context) ([]*dto.MetricFamily, error) {
	p.mu.RLock()
	vmsCopy := make(map[string]*CocoonVM, len(p.vms))
	for k, v := range p.vms {
		vmsCopy[k] = v
	}
	p.mu.RUnlock()

	gauge := dto.MetricType_GAUGE
	families := make([]*dto.MetricFamily, 0, 3)

	// Per-pod CPU and memory metrics
	cpuName := "pod_cpu_usage_seconds_total"
	memName := "pod_memory_working_set_bytes"
	netRxName := "pod_network_receive_bytes_total"
	netTxName := "pod_network_transmit_bytes_total"

	cpuMetrics := make([]*dto.Metric, 0)
	memMetrics := make([]*dto.Metric, 0)
	netRxMetrics := make([]*dto.Metric, 0)
	netTxMetrics := make([]*dto.Metric, 0)

	for key, vm := range vmsCopy {
		parts := strings.SplitN(key, "/", 2)
		if len(parts) != 2 || vm.VMID == "" {
			continue
		}
		ns, name := parts[0], parts[1]
		nsLabel := "namespace"
		podLabel := "pod"
		labels := []*dto.LabelPair{
			{Name: &nsLabel, Value: &ns},
			{Name: &podLabel, Value: &name},
		}

		if !strings.HasPrefix(vm.VMID, "static-") {
			if ping, err := chGetPing(vm.VMID); err == nil && ping.PID > 0 {
				if uNs, sNs, err := readProcCPUUsage(ping.PID); err == nil {
					cpuSec := float64(uNs+sNs) / 1e9
					cpuMetrics = append(cpuMetrics, &dto.Metric{Label: labels, Gauge: &dto.Gauge{Value: &cpuSec}})
				}
				if rss, err := readProcMemoryRSS(ping.PID); err == nil {
					memF := float64(rss)
					memMetrics = append(memMetrics, &dto.Metric{Label: labels, Gauge: &dto.Gauge{Value: &memF}})
				}
			}
			if counters, err := chGetCounters(vm.VMID); err == nil {
				for devName, stats := range counters {
					if strings.Contains(devName, "net") {
						rx := float64(stats["rx_bytes"])
						tx := float64(stats["tx_bytes"])
						if rx < 1e18 { // skip uninitialized u64::MAX
							netRxMetrics = append(netRxMetrics, &dto.Metric{Label: labels, Gauge: &dto.Gauge{Value: &rx}})
						}
						if tx < 1e18 {
							netTxMetrics = append(netTxMetrics, &dto.Metric{Label: labels, Gauge: &dto.Gauge{Value: &tx}})
						}
					}
				}
			}
		}
	}

	if len(cpuMetrics) > 0 {
		families = append(families, &dto.MetricFamily{Name: &cpuName, Type: &gauge, Metric: cpuMetrics})
	}
	if len(memMetrics) > 0 {
		families = append(families, &dto.MetricFamily{Name: &memName, Type: &gauge, Metric: memMetrics})
	}
	if len(netRxMetrics) > 0 {
		families = append(families, &dto.MetricFamily{Name: &netRxName, Type: &gauge, Metric: netRxMetrics})
	}
	if len(netTxMetrics) > 0 {
		families = append(families, &dto.MetricFamily{Name: &netTxName, Type: &gauge, Metric: netTxMetrics})
	}
	return families, nil
}

// ---------- PodNotifier ----------

// NotifyPods implements node.PodNotifier — registers async status callback.
func (p *CocoonProvider) NotifyPods(_ context.Context, cb func(*corev1.Pod)) {
	p.notifyPodCb = cb
	klog.Info("PodNotifier registered — async pod status updates enabled")
}

// notifyPodStatus pushes a pod status update to the VK pod controller.
func (p *CocoonProvider) notifyPodStatus(ns, name string) {
	if p.notifyPodCb == nil {
		klog.V(2).Infof("notifyPodStatus %s/%s: callback is nil", ns, name)
		return
	}
	p.mu.RLock()
	pod, ok := p.pods[podKey(ns, name)]
	p.mu.RUnlock()
	if !ok {
		klog.V(2).Infof("notifyPodStatus %s/%s: pod not found in store", ns, name)
		return
	}
	status, err := p.GetPodStatus(context.Background(), ns, name)
	if err != nil {
		klog.Warningf("notifyPodStatus %s/%s: GetPodStatus: %v", ns, name, err)
		return
	}
	podCopy := pod.DeepCopy()
	podCopy.Status = *status
	klog.Infof("notifyPodStatus %s/%s: phase=%s ready=%v containers=%d",
		ns, name, status.Phase,
		len(status.Conditions) > 0 && status.Conditions[0].Status == "True",
		len(status.ContainerStatuses))
	p.notifyPodCb(podCopy)
}

// ---------- Post-clone network fix ----------

// ---------- Internal helpers ----------

func (p *CocoonProvider) getVM(ns, name string) *CocoonVM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.vms[podKey(ns, name)]
}

func (p *CocoonProvider) sshPass(vm *CocoonVM) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if pod, ok := p.pods[podKey(vm.PodNamespace, vm.PodName)]; ok {
		if pw := ann(pod, AnnSSHPassword, ""); pw != "" {
			return pw
		}
	}
	return p.sshPassword
}

var cocoonPassThroughEnv = []string{
	"COCOON_ROOT_DIR",
	"COCOON_RUN_DIR",
	"COCOON_LOG_DIR",
	"COCOON_CNI_CONF_DIR",
	"COCOON_CNI_BIN_DIR",
	"COCOON_DNS",
	"COCOON_BALLOON_ENABLED",
	"COCOON_ROOT_PASSWORD",
	"COCOON_CH_BINARY",
	"COCOON_WINDOWS_CH_BINARY",
}

func (p *CocoonProvider) cocoonExec(ctx context.Context, args ...string) (string, error) {
	if p.cocoonExecFn != nil {
		return p.cocoonExecFn(ctx, args...)
	}
	sudoArgs := make([]string, 0, len(args)+len(cocoonPassThroughEnv)+2)
	var envArgs []string
	for _, key := range cocoonPassThroughEnv {
		if val := strings.TrimSpace(os.Getenv(key)); val != "" {
			envArgs = append(envArgs, key+"="+val)
		}
	}
	if len(envArgs) > 0 {
		sudoArgs = append(sudoArgs, "env")
		sudoArgs = append(sudoArgs, envArgs...)
	}
	sudoArgs = append(sudoArgs, p.cocoonBin)
	sudoArgs = append(sudoArgs, args...)
	cmd := exec.CommandContext(ctx, "sudo", sudoArgs...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// discoverVM finds a VM by name using `cocoon vm list --format json`.
func (p *CocoonProvider) discoverVM(ctx context.Context, name string) *CocoonVM {
	if p.discoverVMFn != nil {
		return p.discoverVMFn(ctx, name)
	}
	out, err := p.cocoonExec(ctx, "vm", "list", "--format", "json")
	if err != nil {
		// Fallback to text parsing
		return p.discoverVMText(ctx, name)
	}
	var vms []cocoonVMJSON
	if err := json.Unmarshal([]byte(out), &vms); err != nil {
		return p.discoverVMText(ctx, name)
	}
	for _, v := range vms {
		if v.Name == name || v.Config.Name == name {
			return jsonToVM(v)
		}
	}
	return nil
}

// discoverVMByID finds a VM by ID.
// Prefer JSON so MAC and DHCP-resolved guest IP are preserved; fall back to
// text parsing when JSON is unavailable.
func (p *CocoonProvider) discoverVMByID(ctx context.Context, vmID string) *CocoonVM {
	if p.discoverVMByIDFn != nil {
		return p.discoverVMByIDFn(ctx, vmID)
	}
	if out, err := p.cocoonExec(ctx, "vm", "list", "--format", "json"); err == nil {
		var vms []cocoonVMJSON
		if err := json.Unmarshal([]byte(out), &vms); err == nil {
			for _, v := range vms {
				if v.ID == vmID || strings.HasPrefix(v.ID, vmID) {
					return jsonToVM(v)
				}
			}
		}
	}
	out, _ := p.cocoonExec(ctx, "vm", "list")
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, vmID) {
			continue
		}
		// Parse: ID  NAME  STATE  CPU  MEMORY  STORAGE  IP  IMAGE  CREATED
		// STATE can be multi-word: "stopped (stale)", "running"
		// Strategy: extract ID and NAME (first 2 fields), then find IP by pattern
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		name := f[1]
		// Determine state: everything between name and the CPU number
		state := "unknown"
		if strings.Contains(line, "stopped") && strings.Contains(line, "stale") {
			state = "stopped (stale)"
		} else if strings.Contains(line, "running") {
			state = "running"
		} else if strings.Contains(line, "creating") {
			state = "creating"
		} else if strings.Contains(line, "stopped") {
			state = "stopped"
		}
		// Find IP: look for 10.88.x.x pattern
		ip := ""
		for _, field := range f {
			if strings.HasPrefix(field, "10.88.") {
				ip = field
				break
			}
		}
		return &CocoonVM{VMID: vmID, VMName: name, State: state, IP: ip}
	}
	return nil
}

func (p *CocoonProvider) discoverVMText(ctx context.Context, name string) *CocoonVM {
	out, _ := p.cocoonExec(ctx, "vm", "list")
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 6 && f[1] == name {
			ip := f[5]
			if ip == "-" {
				ip = ""
			}
			return &CocoonVM{VMID: f[0], VMName: f[1], State: f[2], IP: ip, CPU: 2, MemoryMB: 8192}
		}
	}
	return nil
}

func jsonToVM(v cocoonVMJSON) *CocoonVM {
	cniIP := v.IP
	mac := ""
	if len(v.NetworkConfigs) > 0 {
		if v.NetworkConfigs[0].Network.IP != "" {
			cniIP = v.NetworkConfigs[0].Network.IP
		}
		mac = v.NetworkConfigs[0].MAC
	}
	if cniIP == "-" || cniIP == "" {
		cniIP = ""
	}
	// Prefer DHCP lease IP over CNI IP (guest may use DHCP with a different IP)
	ip := cniIP
	if mac != "" {
		if dhcpIP := resolveIPFromLeaseByMAC(mac); dhcpIP != "" {
			ip = dhcpIP
		}
	}
	memMB := int(v.Memory / (1024 * 1024))
	if memMB == 0 && v.Config.Memory > 0 {
		memMB = int(v.Config.Memory / (1024 * 1024))
	}
	cpu := v.CPU
	if cpu == 0 {
		cpu = v.Config.CPU
	}
	vmName := v.Name
	if vmName == "" {
		vmName = v.Config.Name
	}
	return &CocoonVM{
		VMID:     v.ID,
		VMName:   vmName,
		State:    v.State,
		IP:       ip,
		MAC:      mac,
		CPU:      cpu,
		MemoryMB: memMB,
		Image:    v.Image,
	}
}

// resolveIPFromLeaseByMAC is a standalone helper (no freshness check, for reconcile).
func resolveIPFromLeaseByMAC(mac string) string {
	return resolveLeaseByMAC(mac, time.Time{})
}

// waitForDHCPIP polls dnsmasq leases until a DHCP IP (10.88.100.x) appears for the VM.
// Only accepts leases newer than the VM creation time to avoid stale entries.
func (p *CocoonProvider) waitForDHCPIP(ctx context.Context, vm *CocoonVM, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	mac := vm.MAC
	notBefore := time.Now().Add(-60 * time.Second) // lease must be recent
	klog.Infof("waitForDHCPIP: VM %s mac=%s, polling leases (timeout %s)", vm.VMName, mac, timeout)
	for time.Now().Before(deadline) {
		if mac != "" {
			if ip := resolveLeaseByMAC(mac, notBefore); ip != "" && strings.HasPrefix(ip, "10.88.100.") {
				klog.Infof("waitForDHCPIP: VM %s got DHCP IP %s (by MAC)", vm.VMName, ip)
				return ip
			}
		}
		time.Sleep(2 * time.Second)
	}
	klog.Warningf("waitForDHCPIP: VM %s DHCP timeout, falling back to %s", vm.VMName, vm.IP)
	return vm.IP
}

// resolveLeaseByMAC finds the DHCP IP for a MAC address.
// If notBefore is non-zero, only returns leases with timestamp >= notBefore.
func resolveLeaseByMAC(mac string, notBefore time.Time) string {
	data, _ := os.ReadFile("/var/lib/misc/dnsmasq.leases")
	minTS := notBefore.Unix()
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		// Format: timestamp mac ip hostname clientid
		f := strings.Fields(sc.Text())
		if len(f) >= 3 && f[1] == mac {
			if minTS > 0 {
				ts, _ := strconv.ParseInt(f[0], 10, 64)
				if ts < minTS {
					continue // stale lease
				}
			}
			return f[2]
		}
	}
	return ""
}

// resolveIPFromLease reads dnsmasq leases to find IP by hostname or MAC (no freshness check).
func (p *CocoonProvider) resolveIPFromLease(hostnameOrMAC string) string {
	data, _ := os.ReadFile("/var/lib/misc/dnsmasq.leases")
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 4 && (f[3] == hostnameOrMAC || f[1] == hostnameOrMAC) {
			return f[2]
		}
	}
	return ""
}

// shellQuoteJoin joins command args into a single shell-safe string.
// For SSH, the remote command is passed as a single string to the remote shell.
func shellQuoteJoin(args []string) string {
	if len(args) == 0 {
		return ""
	}
	// If the command is "sh -c <script>", pass the script directly
	if len(args) >= 3 && args[0] == "sh" && args[1] == "-c" {
		return strings.Join(args[2:], " ")
	}
	// For simple commands like "hostname", "uptime", "bash -l"
	quoted := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t'\"\\$(){}|&;<>!") {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
		} else {
			quoted[i] = a
		}
	}
	return strings.Join(quoted, " ")
}

// NotFound satisfies the errdefs interface.
type notFoundError struct{ msg string }

func (e *notFoundError) Error() string  { return e.msg }
func (e *notFoundError) NotFound() bool { return true }

// NodeCapacity returns the resource capacity for the virtual node.
// Reads real CPU/MEM from host /proc (like kubelet cAdvisor).
func NodeCapacity() corev1.ResourceList {
	cpuCount := readHostCPUCount()
	if cpuCount <= 0 {
		cpuCount = 256 // fallback
	}
	memBytes := readHostMemoryBytes()
	if memBytes == 0 {
		memBytes = 1536 * 1024 * 1024 * 1024 // fallback 1536Gi
	}
	return corev1.ResourceList{
		corev1.ResourceCPU:              *resource.NewQuantity(int64(cpuCount), resource.DecimalSI),
		corev1.ResourceMemory:           *resource.NewQuantity(int64(memBytes), resource.BinarySI),
		corev1.ResourceEphemeralStorage: *resource.NewQuantity(3500*1024*1024*1024, resource.BinarySI), // /data01
		corev1.ResourcePods:             *resource.NewQuantity(241, resource.DecimalSI),
	}
}
