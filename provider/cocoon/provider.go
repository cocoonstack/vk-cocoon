package cocoon

import (
	"context"
	"fmt"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/guest"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/network"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/snapshots"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// compile-time interface check.
var _ provider.Provider = (*Provider)(nil)

const (
	// restartCooldown prevents tight restart loops when a VM keeps crashing.
	restartCooldown = 30 * time.Second

	// containerName is the synthetic container name used in pod status and metrics.
	containerName = "agent"
)

// Provider maps Kubernetes pods to cocoon MicroVMs.
type Provider struct {
	NodeName string

	Clientset    kubernetes.Interface
	Runtime      vm.Runtime
	Puller       *snapshots.Puller
	Pusher       *snapshots.Pusher
	Registry     snapshots.RegistryClient
	LeaseParser  *network.LeaseParser
	Pinger       network.Pinger
	GuestSSH     guest.Executor
	GuestRDP     guest.Executor
	GuestSAC     guest.Dialer
	Probes       *probes.Manager
	OrphanPolicy provider.OrphanPolicy

	startTime   time.Time
	mu          sync.RWMutex
	pods        map[string]*corev1.Pod
	vmsByPod    map[string]*vm.VM
	vmsByName   map[string]*vm.VM
	lastRestart map[string]time.Time // key=vmID, cooldown for restart loops
	notifyHook  func(*corev1.Pod)
}

// NewProvider constructs a Provider with empty tables.
// Default Pinger is NopPinger so tests degrade gracefully.
func NewProvider() *Provider {
	return &Provider{
		startTime:    time.Now(),
		OrphanPolicy: provider.OrphanAlert,
		Pinger:       network.NopPinger{},
		pods:         map[string]*corev1.Pod{},
		vmsByPod:     map[string]*vm.VM{},
		vmsByName:    map[string]*vm.VM{},
		lastRestart:  map[string]time.Time{},
	}
}

// Close releases resources held by the provider.
func (p *Provider) Close() {
	if p.Probes != nil {
		p.Probes.Close()
	}
}

// GetPod returns a deep copy of the stored pod.
func (p *Provider) GetPod(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pod, ok := p.pods[meta.PodKey(namespace, name)]
	if !ok {
		return nil, fmt.Errorf("pod %s/%s not found", namespace, name)
	}
	return pod.DeepCopy(), nil
}

// GetPods returns every pod the provider owns.
func (p *Provider) GetPods(_ context.Context) ([]*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Collect(maps.Values(p.pods)), nil
}

// NotifyPods stores the kubelet's pod-status callback.
func (p *Provider) NotifyPods(_ context.Context, notifier func(*corev1.Pod)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notifyHook = notifier
}

// notify pushes a pod status update through the kubelet callback.
func (p *Provider) notify(pod *corev1.Pod) {
	p.mu.RLock()
	hook := p.notifyHook
	p.mu.RUnlock()
	if hook != nil {
		hook(pod)
	}
}

// trackPod stores the pod and its VM in the in-memory tables.
func (p *Provider) trackPod(pod *corev1.Pod, v *vm.VM) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.pods[key] = pod
	if v != nil {
		p.vmsByPod[key] = v
		if v.Name != "" {
			p.vmsByName[v.Name] = v
		}
	}
	metrics.VMTableSize.Set(float64(len(p.vmsByPod)))
}

// dropVMLocked removes the VM record for key. Caller must hold p.mu for writing.
func (p *Provider) dropVMLocked(key string) {
	v, ok := p.vmsByPod[key]
	if !ok {
		return
	}
	delete(p.lastRestart, v.ID)
	if v.Name != "" {
		delete(p.vmsByName, v.Name)
	}
	delete(p.vmsByPod, key)
	metrics.VMTableSize.Set(float64(len(p.vmsByPod)))
}

func (p *Provider) gcStaleRestarts() {
	p.mu.Lock()
	defer p.mu.Unlock()
	cutoff := time.Now().Add(-restartCooldown * 2)
	for id, t := range p.lastRestart {
		if t.Before(cutoff) {
			delete(p.lastRestart, id)
		}
	}
}

// forgetPod drops the pod, VM, and probe from the in-memory tables.
func (p *Provider) forgetPod(namespace, name string) {
	key := meta.PodKey(namespace, name)
	p.mu.Lock()
	p.dropVMLocked(key)
	delete(p.pods, key)
	p.mu.Unlock()
	if p.Probes != nil {
		p.Probes.Forget(key)
	}
}

// vmForPod returns the VM associated with a pod, or nil.
func (p *Provider) vmForPod(namespace, name string) *vm.VM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.vmsByPod[meta.PodKey(namespace, name)]
}

// setVMIP updates the tracked VM's IP (copy-on-write for concurrency safety).
func (p *Provider) setVMIP(namespace, name, ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := meta.PodKey(namespace, name)
	v, ok := p.vmsByPod[key]
	if !ok {
		return
	}
	updated := *v
	updated.IP = ip
	p.vmsByPod[key] = &updated
	if updated.Name != "" {
		p.vmsByName[updated.Name] = &updated
	}
}

// resolveVMIP returns the VM's IP, falling back to a cocoon-net lease
// lookup when the IP is unknown but a MAC is available.
func (p *Provider) resolveVMIP(namespace, name string, v *vm.VM) string {
	ip := v.IP
	if ip != "" || v.MAC == "" || p.LeaseParser == nil {
		return ip
	}
	lease, err := p.LeaseParser.LookupByMAC(v.MAC)
	if err != nil {
		return ""
	}
	p.setVMIP(namespace, name, lease.IP)
	return lease.IP
}

// buildProbe returns a probe closure that resolves the VM's IP and pings it.
// ICMP works for both Linux and Windows guests.
func (p *Provider) buildProbe(namespace, name string) probes.Probe {
	return func(ctx context.Context) (bool, string) {
		v := p.vmForPod(namespace, name)
		if v == nil {
			return false, "vm gone"
		}
		ip := p.resolveVMIP(namespace, name, v)
		if ip == "" {
			return false, "waiting for dhcp lease"
		}
		if port := p.probePort(namespace, name); port != "" {
			return p.probeTCP(ctx, ip, port)
		}
		if err := p.Pinger.Ping(ctx, ip); err != nil {
			return false, "ping failed: " + err.Error()
		}
		return true, "ping ok"
	}
}

func (p *Provider) probePort(namespace, name string) string {
	pod, _ := p.GetPod(context.Background(), namespace, name)
	if pod == nil {
		return ""
	}
	return pod.Annotations[meta.AnnotationProbePort]
}

func (p *Provider) probeTCP(ctx context.Context, ip, port string) (bool, string) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, port))
	if err != nil {
		return false, "tcp probe " + port + ": " + err.Error()
	}
	_ = conn.Close()
	return true, "tcp ok"
}

// StartVMWatcher launches a background goroutine that subscribes to cocoon's
// VM event stream and reacts to VM state changes in near-real-time.
func (p *Provider) StartVMWatcher(ctx context.Context) {
	go p.vmWatchLoop(ctx)
}

// vmWatchLoop runs the cocoon event stream with automatic restart on failure.
func (p *Provider) vmWatchLoop(ctx context.Context) {
	logger := log.WithFunc("Provider.vmWatchLoop")
	backoff := time.Second
	for {
		events, err := p.Runtime.WatchEvents(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Warnf(ctx, "vm watcher start failed: %v, retrying in %s", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = min(backoff*2, 60*time.Second)
				continue
			}
		}
		backoff = time.Second
		logger.Info(ctx, "vm event watcher started")
		for ev := range events {
			switch ev.Event {
			case "DELETED":
				p.handleVMGone(ctx, &ev.VM)
			case "MODIFIED":
				if ev.VM.State != vm.StateRunning {
					p.handleVMGone(ctx, &ev.VM)
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		logger.Warn(ctx, "vm event watcher exited, restarting in 2s")
		p.gcStaleRestarts()
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// handleVMGone processes a DELETED or MODIFIED(stopped/error) event.
// It double-checks via `cocoon vm inspect` before acting to avoid
// false positives from transient states (e.g. watchdog restart).
//
// - VM gone (inspect fails): delete pod → operator recreates.
// - VM stopped/error: restart via `cocoon vm start` → probe re-pings.
// - VM still running: ignore (false alarm).
func (p *Provider) handleVMGone(ctx context.Context, eventVM *vm.VM) {
	logger := log.WithFunc("Provider.handleVMGone")

	// Find the pod tracking this VM.
	p.mu.RLock()
	var affectedKey string
	var affectedPod *corev1.Pod
	var trackedID string
	for key, tracked := range p.vmsByPod {
		if tracked.ID == eventVM.ID || (tracked.Name != "" && tracked.Name == eventVM.Name) {
			affectedKey = key
			affectedPod = p.pods[key]
			trackedID = tracked.ID
			break
		}
	}
	p.mu.RUnlock()

	if affectedKey == "" || affectedPod == nil {
		return
	}

	// Double-check: inspect the VM via cocoon CLI.
	inspected, err := p.Runtime.Inspect(ctx, trackedID)
	switch {
	case err != nil:
		// VM not found → truly gone. Delete pod so operator recreates.
		logger.Infof(ctx, "vm %s confirmed gone (inspect: %v), deleting pod %s/%s",
			trackedID, err, affectedPod.Namespace, affectedPod.Name)
		p.evictPod(ctx, affectedKey, affectedPod, corev1.PodFailed, "VMGone", "vm no longer exists")

	case inspected.State == vm.StateRunning:
		// Still running → false alarm.
		logger.Debugf(ctx, "vm %s still running after event, ignoring", trackedID)

	default:
		// Stopped/error → restart the CH process in place, with cooldown.
		p.mu.Lock()
		last := p.lastRestart[trackedID]
		cooldownElapsed := time.Since(last) >= restartCooldown
		if cooldownElapsed {
			p.lastRestart[trackedID] = time.Now()
		}
		p.mu.Unlock()
		if !cooldownElapsed {
			logger.Warnf(ctx, "vm %s state=%s, restart cooldown not elapsed, removing VM and evicting pod", trackedID, inspected.State)
			if err := p.Runtime.Remove(ctx, trackedID); err != nil {
				// Remove failed — the VM still exists in an unknown state.
				// Keep the pod so the user/operator can investigate; evicting
				// would delete the pod while the orphan VM remains, causing
				// name collisions when the operator recreates.
				logger.Errorf(ctx, err, "remove vm %s during cooldown eviction, keeping pod for investigation", trackedID)
				return
			}
			p.evictPod(ctx, affectedKey, affectedPod, corev1.PodFailed, "RestartCooldown", "restart cooldown not elapsed")
			return
		}
		logger.Infof(ctx, "vm %s state=%s, restarting", trackedID, inspected.State)
		if startErr := p.Runtime.Start(ctx, trackedID); startErr != nil {
			logger.Errorf(ctx, startErr, "restart vm %s failed, removing VM and evicting pod", trackedID)
			if removeErr := p.Runtime.Remove(ctx, trackedID); removeErr != nil {
				// Remove failed — the VM still exists in an unknown state.
				// Keep the pod so the user/operator can investigate; evicting
				// would delete the pod while the orphan VM remains, causing
				// name collisions when the operator recreates.
				logger.Errorf(ctx, removeErr, "remove vm %s after failed restart, keeping pod for investigation", trackedID)
				return
			}
			p.evictPod(ctx, affectedKey, affectedPod, corev1.PodFailed, "RestartFailed", startErr.Error())
			return
		}
		// Re-inspect to refresh PID and NetworkConfigs for stats collection.
		if fresh, inspectErr := p.Runtime.Inspect(ctx, trackedID); inspectErr == nil {
			p.mu.Lock()
			if old := p.vmsByPod[affectedKey]; old != nil {
				old.PID = fresh.PID
				old.NetworkConfigs = fresh.NetworkConfigs
			}
			p.mu.Unlock()
		}
	}
}

// evictPod removes the VM record, deletes the pod from the API server,
// and notifies the framework so the operator can recreate the pod.
func (p *Provider) evictPod(ctx context.Context, key string, pod *corev1.Pod, phase corev1.PodPhase, reason, message string) {
	logger := log.WithFunc("Provider.evictPod")

	// Detach before mutating status to avoid a data race with concurrent readers.
	p.mu.Lock()
	p.dropVMLocked(key)
	delete(p.pods, key)
	p.mu.Unlock()

	if p.Probes != nil {
		p.Probes.Forget(key)
	}

	if p.Clientset != nil {
		if err := p.Clientset.CoreV1().Pods(pod.Namespace).Delete(
			ctx, pod.Name, metav1.DeleteOptions{},
		); err != nil {
			logger.Warnf(ctx, "delete pod %s/%s: %v (phase kept as-is)", pod.Namespace, pod.Name, err)
			p.notify(pod)
			return
		}
	}

	pod.Status.Phase = phase
	if phase == corev1.PodFailed && reason != "" {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{
			{
				Name: containerName,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1,
						Reason:   reason,
						Message:  message,
					},
				},
			},
		}
	}
	p.notify(pod)
}

// patchPodAnnotations patches the given annotations onto a pod via the API server.
func (p *Provider) patchPodAnnotations(ctx context.Context, namespace, name string, annos map[string]any) error {
	if p.Clientset == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	patch, err := commonk8s.AnnotationsMergePatch(annos)
	if err != nil {
		return fmt.Errorf("marshal annotations %s/%s: %w", namespace, name, err)
	}
	if _, err := p.Clientset.CoreV1().Pods(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch annotations %s/%s: %w", namespace, name, err)
	}
	return nil
}

// clearRuntimeAnnotations removes VMID/IP from the pod's in-memory
// annotations and patches the API server. Used by hibernate and startup
// reconcile to clear stale runtime state.
func (p *Provider) clearRuntimeAnnotations(ctx context.Context, pod *corev1.Pod) error {
	if pod.Annotations != nil {
		delete(pod.Annotations, meta.AnnotationVMID)
		delete(pod.Annotations, meta.AnnotationIP)
	}
	return p.patchPodAnnotations(ctx, pod.Namespace, pod.Name, map[string]any{
		meta.AnnotationVMID: nil,
		meta.AnnotationIP:   nil,
	})
}

// buildOnUpdate returns the callback invoked on readiness transitions.
// Under the async provider contract this is the only way status changes reach the kubelet.
func (p *Provider) buildOnUpdate(namespace, name string) probes.OnUpdate {
	return func(ctx context.Context) {
		pod, err := p.GetPod(ctx, namespace, name)
		if err != nil {
			log.WithFunc("Provider.probeUpdate").
				Warnf(ctx, "pod %s/%s lookup failed, skipping notify: %v", namespace, name, err)
			return
		}
		p.refreshStatus(ctx, pod)
		p.notify(pod)
	}
}
