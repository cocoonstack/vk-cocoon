// Package provider implements a Virtual Kubelet provider that maps
// Kubernetes Pods to Cocoon MicroVMs.
//
// VM lifecycle (snapshot-based):
//
//	CreatePod  → derive stable VM name (with slot for Deployments)
//	           → check ConfigMap for suspended snapshot → pull from epoch
//	           → cocoon vm clone --name <vm> <snapshot> → VM running
//	DeletePod  → detect scale-down vs restart:
//	           → restart/kill: snapshot save → push epoch → record → destroy VM
//	           → scale-down:   clear snapshot record → destroy VM (no snapshot)
//	           → replicas=0:   snapshot all (suspend group)
//
// Annotation reference (set on Pod):
//
//	cocoon.cis/image          — snapshot or image name (default: container image field)
//	cocoon.cis/mode           — "clone" (from snapshot) or "run" (from image). Default: clone
//	cocoon.cis/storage        — COW disk size (e.g. "100G"). Default: linux "100G", windows "15G"
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
	"os"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
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
	AnnNetwork      = "cocoon.cis/network"     // CNI conflist name (empty = cocoon default)
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

// cocoonVMJSON matches legacy `cocoon vm list --format json` output.
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

// cocoonInspectJSON matches newer `cocoon inspect` and `cocoon list --format json`.
type cocoonInspectJSON struct {
	VMID  string `json:"vm_id"`
	Name  string `json:"name"`
	State string `json:"state"`
	Image struct {
		Ref string `json:"ref"`
	} `json:"image"`
	BootConfig struct {
		CPUs     int   `json:"cpus"`
		MemoryMB int64 `json:"memory_mb"`
	} `json:"boot_config"`
	Timestamps struct {
		CreatedAt string `json:"created_at"`
		StartedAt string `json:"started_at"`
	} `json:"timestamps"`
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

	// VM state cache fed by cocoon vm status --event stream.
	vmState *vmCache

	// Persistent pod → VM mapping for crash recovery.
	podMap *podMap

	// Probe state per pod.
	probeStates map[string]*probeResult

	// Test hooks.
	discoverVMFn              func(context.Context, string) *CocoonVM
	discoverVMByIDFn          func(context.Context, string) *CocoonVM
	lookupSuspendedSnapshotFn func(context.Context, string, string) string
	cocoonExecFn              func(context.Context, ...string) (string, error)
	waitForDHCPIPFn           func(context.Context, *CocoonVM, time.Duration) string
	hibernateVMFn             func(context.Context, *corev1.Pod, *CocoonVM)
	wakeVMFn                  func(context.Context, *corev1.Pod, *CocoonVM)
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

	p.podMap = newPodMap()

	// Start VM event stream watcher (inotify-backed, ~200ms latency).
	p.startVMWatcher(ctx)

	// Start reconciliation loop as fallback (10s ticker).
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
	network string
}

func buildRunArgs(rc runConfig) []string {
	args := []string{"run", "--name", rc.vmName, "--cpus", rc.cpu, "--memory", rc.mem, "--disk", rc.storage}
	if rc.network != "" {
		args = append(args, "--network", rc.network)
	}
	return append(args, rc.image)
}

func buildLegacyRunArgs(rc runConfig) []string {
	args := []string{"vm", "run", "--name", rc.vmName, "--cpu", rc.cpu, "--memory", rc.mem, "--storage", rc.storage}
	if rc.nics != "" {
		args = append(args, "--nics", rc.nics)
	}
	if rc.network != "" {
		args = append(args, "--network", rc.network)
	}
	if rc.dns != "" {
		args = append(args, "--dns", rc.dns)
	}
	if rc.rootPwd != "" && rc.osType != osWindows {
		args = append(args, "--default-root-password", rc.rootPwd)
	}
	if rc.osType == osWindows {
		args = append(args, "--windows")
	}
	return append(args, rc.image)
}

func buildCloneArgs(vmName, network, snapshot string) []string {
	args := []string{"vm", "clone", "--name", vmName}
	if network != "" {
		args = append(args, "--network", network)
	}
	return append(args, snapshot)
}
