package cocoon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/oci"
	"github.com/cocoonstack/vk-cocoon/guest"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/network"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/snapshots"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	deleteClaimed deleteClaim = iota
	deleteInFlight
	deleteSuperseded

	// restartCooldown prevents tight restart loops when a VM keeps crashing.
	restartCooldown = 30 * time.Second

	// initialStatusPushDelay outlasts the pod informer's knownPods population, which drops earlier pushes.
	initialStatusPushDelay  = 10 * time.Second
	statusReconcileInterval = 30 * time.Second

	// containerName is the synthetic container name used in pod status and metrics.
	containerName = "agent"

	// evictDeleteAttempts and evictDeleteBaseDelay stay small so a flaky apiserver cannot stall the serialized event loop.
	evictDeleteAttempts  = 2
	evictDeleteBaseDelay = 200 * time.Millisecond

	// inlineInspectAttempts covers one CLI hiccup; the deferred recheck takes over beyond it.
	inlineInspectAttempts = 2

	// startupFanOut and statusReconcileFanOut bound separate fan-outs; equal today, tuned separately.
	startupFanOut         = 8
	statusReconcileFanOut = 8

	// Overridable via Provider fields so tests can shrink them without racing on package globals.
	defaultInlineInspectBaseDelay      = 200 * time.Millisecond
	defaultDeferredRecheckInitialDelay = 1 * time.Second
	defaultDeferredRecheckMaxDelay     = 30 * time.Second

	// defaultDeferredRecheckBudget caps one recheck loop; on timeout the pod is evicted (VMInspectTimeout).
	defaultDeferredRecheckBudget = 30 * time.Minute
)

type podNotifier func(*corev1.Pod)

type deleteClaim int

// Provider maps Kubernetes pods to cocoon MicroVMs.
type Provider struct {
	NodeName                   string
	SnapshotCompatibilityClass string

	OrphanPolicy provider.OrphanPolicy
	RestoreMode  vm.RestoreMode

	Clientset kubernetes.Interface
	Pods      corev1listers.PodLister
	Runtime   vm.Runtime
	// MacosBin is the cocoon-macos binary os=macos pods dispatch to.
	// MacosVNCPassword protects the node-exposed per-VM QEMU VNC ports.
	MacosBin         string
	MacosVNCPassword string
	Puller           *snapshots.Puller
	Pusher           *snapshots.Pusher
	PeerRestorer     *snapshots.PeerRestorer
	PeerPort         string
	Registry         oci.Registry
	LeaseParser      *network.LeaseParser
	LeaseReleaser    network.LeaseReleaser
	Pinger           network.Pinger
	GuestSAC         guest.Dialer
	Probes           *probes.Manager
	Recorder         record.EventRecorder

	startTime time.Time
	//nolint:containedctx // deferred recheck must outlive the watcher ctx (which cycles on event-stream reconnect) and be cancelable only by Close
	lifecycleCtx   context.Context
	lifecycleStop  context.CancelFunc // canceled from Close to stop deferred goroutines
	mu             sync.RWMutex
	pods           map[string]*corev1.Pod
	vmsByPod       map[string]*vm.VM
	vmsByName      map[string]*vm.VM
	macosVNC       map[string]int       // key=pod, node-unique VNC host-port reservations for macOS guests
	lastRestart    map[string]time.Time // key=vmID, cooldown for restart loops
	pendingRecheck map[string]struct{}  // key=vmID, dedup for deferred recheck goroutines
	resumedOps     map[string]struct{}  // key=pod, full ops resumed by dispatchOwedWork; UpdatePod backs off
	recheckWG      sync.WaitGroup       // tracks deferred recheck goroutines so Close can await them
	bgWG           sync.WaitGroup       // tracks per-pod async goroutines (post-clone exec, static-IP) so Close can await them
	forkSnapshotSF singleflight.Group   // dedups concurrent fork-base snapshot creation (self-synchronized)
	snapshotPullSF singleflight.Group   // dedups concurrent registry pulls of one local snapshot name (self-synchronized)
	runImageSF     singleflight.Group   // dedups concurrent base-image materialization of one ref (self-synchronized)
	macosImageSF   singleflight.Group   // dedups concurrent cocoon-macos image pulls of one ref (self-synchronized)
	notifyHook     podNotifier

	// macOS test seams; production leaves them nil (real exec / real signal-0 probe).
	macosExecFn         func(context.Context, ...string) (string, error)
	macosProcessAliveFn func(int) bool
	// Source of truth for lifecycle annotations (decoupled from p.pods).
	lifecycleIntent map[string]lifecycleEntry
	deleting        map[string]struct{}

	// Shared scrape sample; see sampleStats.
	statsMu   sync.Mutex
	statsAt   time.Time
	statsVMs  []vmSample
	statsNode provider.NodeStats

	// Zero values fall back to the defaultXxx constants; tests shrink them before exercising handleVMGone.
	inlineInspectBaseDelay      time.Duration
	deferredRecheckInitialDelay time.Duration
	deferredRecheckMaxDelay     time.Duration
	deferredRecheckBudget       time.Duration

	// dropNIC wake tunables; defaults live in update.go.
	wakeFreshIPBudget   time.Duration
	wakeFreshIPInterval time.Duration
	wakeRenewNudgeDelay time.Duration
}

// NewProvider constructs a Provider with empty tables; background work stops when ctx is canceled or Close is called.
func NewProvider(ctx context.Context) *Provider {
	lifecycleCtx, lifecycleStop := context.WithCancel(ctx)
	return &Provider{
		startTime:       time.Now(),
		lifecycleCtx:    lifecycleCtx,
		lifecycleStop:   lifecycleStop,
		OrphanPolicy:    provider.OrphanDestroy,
		RestoreMode:     vm.RestoreMmap,
		Pinger:          network.NopPinger{},
		pods:            map[string]*corev1.Pod{},
		vmsByPod:        map[string]*vm.VM{},
		vmsByName:       map[string]*vm.VM{},
		macosVNC:        map[string]int{},
		lastRestart:     map[string]time.Time{},
		pendingRecheck:  map[string]struct{}{},
		resumedOps:      map[string]struct{}{},
		lifecycleIntent: map[string]lifecycleEntry{},
		deleting:        map[string]struct{}{},
	}
}

// Close cancels background goroutines under p.mu, so no spawn path can Add to a waitgroup after Wait returned.
func (p *Provider) Close() {
	p.mu.Lock()
	p.lifecycleStop()
	p.mu.Unlock()
	p.recheckWG.Wait()
	p.bgWG.Wait()
	if p.Probes != nil {
		p.Probes.Close()
	}
}

func (p *Provider) GetPod(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pod, ok := p.pods[meta.PodKey(namespace, name)]
	if !ok {
		return nil, fmt.Errorf("pod %s/%s not found", namespace, name)
	}
	return pod.DeepCopy(), nil
}

func (p *Provider) GetPods(_ context.Context) ([]*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pods := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		pods = append(pods, pod.DeepCopy())
	}
	return pods, nil
}

func (p *Provider) NotifyPods(_ context.Context, notifier func(*corev1.Pod)) {
	p.mu.Lock()
	p.notifyHook = notifier
	p.mu.Unlock()
	p.goBackground(func() {
		p.runStatusReconciler(p.lifecycleCtx)
	})
}

// StartVMWatcher subscribes to cocoon's VM event stream in the background.
func (p *Provider) StartVMWatcher(ctx context.Context) {
	go p.vmWatchLoop(ctx)
}

// goBackground spawns f under p.mu so bgWG.Go cannot race Close's Wait (add-after-wait misuse).
func (p *Provider) goBackground(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lifecycleCtx.Err() != nil {
		return
	}
	p.bgWG.Go(f)
}

// runStatusReconciler repairs endpoint annotations and status pushes dropped during startup or an apiserver outage.
func (p *Provider) runStatusReconciler(ctx context.Context) {
	if !commonk8s.SleepCtx(ctx, initialStatusPushDelay) {
		return
	}
	p.reconcilePodStatuses(ctx)

	commonk8s.RunTicker(ctx, statusReconcileInterval, p.reconcilePodStatuses)
}

func (p *Provider) reconcilePodStatuses(ctx context.Context) {
	p.mu.RLock()
	pods := slices.Collect(maps.Values(p.pods))
	p.mu.RUnlock()
	if len(pods) == 0 {
		return
	}
	logger := log.WithFunc("Provider.reconcilePodStatuses")
	fanOut(statusReconcileFanOut, pods, func(pod *corev1.Pod) {
		current, err := p.currentPod(ctx, pod)
		if err != nil {
			logger.Errorf(ctx, err, "get pod %s/%s for status reconciliation", pod.Namespace, pod.Name)
			return
		}
		if current.UID != pod.UID {
			return
		}
		status, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name)
		if err != nil {
			logger.Errorf(ctx, err, "derive pod %s/%s status", pod.Namespace, pod.Name)
			return
		}
		key := meta.PodKey(pod.Namespace, pod.Name)
		if !p.trackedPodMatches(key, pod.UID) {
			return
		}
		if !p.reconcileRuntimeEndpoints(ctx, current, status.PodIP) {
			return
		}
		if podStatusMatches(current.Status, *status) {
			return
		}
		if !p.trackedPodMatches(key, pod.UID) {
			return
		}
		current.Status = *status
		logger.Infof(ctx, "republishing drifted status for pod %s/%s", pod.Namespace, pod.Name)
		p.notify(current)
	})
}

// currentPod reads the framework's node-filtered lister; the apiserver GET is the test seam.
func (p *Provider) currentPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	if p.Pods == nil {
		return p.Clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	}
	current, err := p.Pods.Pods(pod.Namespace).Get(pod.Name)
	if err != nil {
		return nil, err
	}
	return current.DeepCopy(), nil
}

// notify hands the framework a copy taken under p.mu: vk DeepCopies the pointer only at drain time.
func (p *Provider) notify(pod *corev1.Pod) {
	p.mu.RLock()
	hook := p.notifyHook
	handoff := pod.DeepCopy()
	p.mu.RUnlock()
	if hook != nil {
		hook(handoff)
	}
}

func (p *Provider) trackPod(pod *corev1.Pod, v *vm.VM) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.trackPodLocked(pod, v)
}

func (p *Provider) trackPodUnlessDeleting(pod *corev1.Pod, v *vm.VM) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deletingLocked(meta.PodKey(pod.Namespace, pod.Name)) {
		return false
	}
	p.trackPodLocked(pod, v)
	return true
}

func (p *Provider) trackPodIncarnation(pod *corev1.Pod, v *vm.VM) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := meta.PodKey(pod.Namespace, pod.Name)
	if p.deletingLocked(key) || p.supersededLocked(key, pod.UID) {
		p.indexOrphanByNameLocked(v)
		return false
	}
	p.trackPodLocked(pod, v)
	return true
}

func (p *Provider) trackPodLocked(pod *corev1.Pod, v *vm.VM) {
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.reassertLifecycleLocked(key, pod)
	p.pods[key] = pod
	if v != nil {
		p.setVMLocked(key, v)
	}
	metrics.VMTableSize.Set(float64(len(p.vmsByPod)))
}

// setVMLocked writes v into both VM tables under p.mu; the write half of dropVMLocked.
func (p *Provider) setVMLocked(key string, v *vm.VM) {
	p.vmsByPod[key] = v
	if v.Name != "" {
		p.vmsByName[v.Name] = v
	}
}

// dropVMLocked removes the VM record for key. Caller must hold p.mu for writing.
func (p *Provider) dropVMLocked(key string) {
	v, ok := p.vmsByPod[key]
	if !ok {
		return
	}
	delete(p.lastRestart, v.ID)
	if p.vmsByName[v.Name] == v {
		delete(p.vmsByName, v.Name)
	}
	delete(p.vmsByPod, key)
	metrics.VMTableSize.Set(float64(len(p.vmsByPod)))
}

func (p *Provider) gcStaleRestarts() {
	p.mu.Lock()
	defer p.mu.Unlock()
	cutoff := time.Now().Add(-restartCooldown * 2)
	maps.DeleteFunc(p.lastRestart, func(_ string, t time.Time) bool {
		return t.Before(cutoff)
	})
}

func (p *Provider) forgetPod(namespace, name string) {
	p.untrackPod(meta.PodKey(namespace, name))
}

func (p *Provider) untrackPod(key string) {
	p.mu.Lock()
	p.untrackLocked(key)
	p.mu.Unlock()
	if p.Probes != nil {
		p.Probes.Forget(key)
	}
}

func (p *Provider) untrackIncarnation(key string, uid types.UID) {
	p.mu.Lock()
	dropped := p.untrackIncarnationLocked(key, uid)
	p.mu.Unlock()
	if dropped && p.Probes != nil {
		p.Probes.Forget(key)
	}
}

func (p *Provider) detachIncarnation(key string, uid types.UID, v *vm.VM) bool {
	p.mu.Lock()
	if p.deletingLocked(key) {
		p.mu.Unlock()
		return false
	}
	dropped := p.untrackIncarnationLocked(key, uid)
	if bound := p.vmsByPod[key]; v != nil && (bound == nil || bound.Name != v.Name) {
		p.indexOrphanByNameLocked(v)
	}
	p.mu.Unlock()
	if dropped && p.Probes != nil {
		p.Probes.Forget(key)
	}
	return true
}

func (p *Provider) untrackIncarnationLocked(key string, uid types.UID) bool {
	tracked, ok := p.pods[key]
	if !ok || tracked.UID != uid {
		return false
	}
	p.untrackLocked(key)
	return true
}

func (p *Provider) untrackLocked(key string) {
	p.dropVMLocked(key)
	delete(p.pods, key)
	delete(p.macosVNC, key)
	delete(p.lifecycleIntent, key)
}

func (p *Provider) vmForPod(namespace, name string) *vm.VM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.vmsByPod[meta.PodKey(namespace, name)]
}

func (p *Provider) trackedPodUID(key string) (types.UID, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if tracked := p.pods[key]; tracked != nil {
		return tracked.UID, true
	}
	return "", false
}

func (p *Provider) trackedPodMatches(key string, uid types.UID) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	tracked, ok := p.pods[key]
	return ok && tracked.UID == uid
}

func (p *Provider) supersededLocked(key string, uid types.UID) bool {
	tracked := p.pods[key]
	return tracked != nil && tracked.UID != uid
}

func (p *Provider) claimDeletingIncarnation(key string, uid types.UID, vmID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deletingLocked(key) {
		return false
	}
	tracked := p.pods[key]
	v := p.vmsByPod[key]
	if tracked == nil || tracked.UID != uid || v == nil || v.ID != vmID {
		return false
	}
	p.deleting[key] = struct{}{}
	return true
}

func (p *Provider) claimDeleting(key string, uid types.UID) deleteClaim {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.supersededLocked(key, uid) {
		return deleteSuperseded
	}
	if p.deletingLocked(key) {
		return deleteInFlight
	}
	p.deleting[key] = struct{}{}
	return deleteClaimed
}

func (p *Provider) finishDeleting(key string) {
	p.mu.Lock()
	delete(p.deleting, key)
	p.mu.Unlock()
}

func (p *Provider) deletingLocked(key string) bool {
	_, deleting := p.deleting[key]
	return deleting
}

// updateTrackedVM applies mutate copy-on-write and returns nil when the pod's VM changed underneath the caller.
func (p *Provider) updateTrackedVM(namespace, name, vmID string, mutate func(*vm.VM)) *vm.VM {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := meta.PodKey(namespace, name)
	v, ok := p.vmsByPod[key]
	if !ok || v.ID != vmID {
		return nil
	}
	updated := *v
	mutate(&updated)
	p.setVMLocked(key, &updated)
	return &updated
}

// setVMIP updates the tracked VM's IP (copy-on-write for concurrency safety).
func (p *Provider) setVMIP(namespace, name, vmID, ip string) bool {
	return p.updateTrackedVM(namespace, name, vmID, func(v *vm.VM) { v.IP = ip }) != nil
}

// resolveVMIP re-reads the lease each call: cocoon-net can rebind a MAC, so the tracked IP is only a cache (#70).
func (p *Provider) resolveVMIP(namespace, name string, v *vm.VM) string {
	if len(v.NetworkConfigs) > 0 && isStaticNIC(v.NetworkConfigs[0]) {
		return v.IP
	}
	if v.MAC == "" || p.LeaseParser == nil {
		return v.IP
	}
	ip := ""
	switch lease, err := p.LeaseParser.LookupByMAC(v.MAC); {
	case err == nil:
		ip = lease.IP
	case !errors.Is(err, network.ErrNoLease):
		return v.IP
	}
	if ip == v.IP {
		return v.IP
	}
	if !p.setVMIP(namespace, name, v.ID, ip) {
		return ""
	}
	return ip
}

func (p *Provider) seedLeaseIP(v *vm.VM) {
	if v.IP != "" || v.MAC == "" || p.LeaseParser == nil {
		return
	}
	if lease, err := p.LeaseParser.LookupByMAC(v.MAC); err == nil {
		v.IP = lease.IP
	}
}

// buildProbe returns a probe that resolves the VM's IP and pings it; ICMP works for Linux and Windows guests.
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

// probePort reads the annotation under RLock; a DeepCopy per probe tick is not affordable.
func (p *Provider) probePort(namespace, name string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pod, ok := p.pods[meta.PodKey(namespace, name)]
	if !ok {
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
			logger.Errorf(ctx, err, "vm watcher start failed, retrying in %s", backoff)
			if !commonk8s.SleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 60*time.Second)
			continue
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
		if !commonk8s.SleepCtx(ctx, 2*time.Second) {
			return
		}
	}
}

// handleVMGone re-inspects before acting on a DELETED or stopped/error event, so a transient state cannot evict a live pod.
func (p *Provider) handleVMGone(ctx context.Context, eventVM *vm.VM) {
	logger := log.WithFunc("Provider.handleVMGone")

	affectedKey, affectedPod, trackedVM := p.podForVMMatch(eventVM.ID, eventVM.Name)
	if affectedKey == "" || affectedPod == nil {
		return
	}
	trackedID := trackedVM.ID

	// Hibernate's own Runtime.Remove triggers this event; restarting would race the cleanup.
	if meta.ReadHibernateState(affectedPod) {
		logger.Infof(ctx, "vm %s pod %s/%s is hibernating, skipping VM-gone handler",
			trackedID, affectedPod.Namespace, affectedPod.Name)
		return
	}

	p.mu.RLock()
	midDelete := p.deletingLocked(affectedKey)
	p.mu.RUnlock()
	if midDelete {
		logger.Infof(ctx, "vm %s pod %s/%s is being deleted, skipping VM-gone handler",
			trackedID, affectedPod.Namespace, affectedPod.Name)
		return
	}

	inspected, err := p.inspectWithRetry(ctx, trackedID)
	switch {
	case errors.Is(err, vm.ErrVMNotFound):
		logger.Infof(ctx, "vm %s confirmed gone, deleting pod %s/%s",
			trackedID, affectedPod.Namespace, affectedPod.Name)
		p.evictGoneIncarnation(ctx, affectedKey, affectedPod, trackedVM, "VMGone", "vm no longer exists")

	case err != nil:
		// cocoon does not re-emit DELETED and probes only ping IPs, so a deferred recheck must settle a still-transient VM
		logger.Errorf(ctx, err, "inspect vm %s inconclusive, scheduling deferred recheck for pod %s/%s",
			trackedID, affectedPod.Namespace, affectedPod.Name)
		metrics.VMInspectTransientFailTotal.Inc()
		p.scheduleDeferredRecheck(trackedID)

	case inspected.State == vm.StateRunning:
		logger.Debugf(ctx, "vm %s still running after event, ignoring", trackedID)

	default:
		p.mu.Lock()
		last := p.lastRestart[trackedID]
		cooldownElapsed := time.Since(last) >= restartCooldown
		if cooldownElapsed {
			p.lastRestart[trackedID] = time.Now()
		}
		p.mu.Unlock()
		if !cooldownElapsed {
			logger.Warnf(ctx, "vm %s state=%s, restart cooldown not elapsed, removing VM and evicting pod", trackedID, inspected.State)
			p.removeThenEvict(ctx, inspected, affectedKey, affectedPod, "RestartCooldown", "restart cooldown not elapsed")
			return
		}
		logger.Infof(ctx, "vm %s state=%s, restarting", trackedID, inspected.State)
		if startErr := p.Runtime.Start(ctx, trackedID); startErr != nil {
			logger.Errorf(ctx, startErr, "restart vm %s failed, removing VM and evicting pod", trackedID)
			p.removeThenEvict(ctx, inspected, affectedKey, affectedPod, "RestartFailed", startErr.Error())
			return
		}
		if fresh, inspectErr := p.Runtime.Inspect(ctx, trackedID); inspectErr == nil {
			p.updateTrackedVM(affectedPod.Namespace, affectedPod.Name, trackedID, func(v *vm.VM) {
				v.PID, v.NetworkConfigs = fresh.PID, fresh.NetworkConfigs
			})
		}
	}
}

// removeThenEvict keeps the pod when the VM remove fails: evicting would orphan the live VM and collide on recreate.
func (p *Provider) removeThenEvict(ctx context.Context, v *vm.VM, key string, pod *corev1.Pod, reason, message string) {
	logger := log.WithFunc("Provider.removeThenEvict")
	if !p.claimDeletingIncarnation(key, pod.UID, v.ID) {
		logger.Infof(ctx, "vm %s tracking changed before %s eviction of pod %s/%s, skipping",
			v.ID, reason, pod.Namespace, pod.Name)
		return
	}
	defer p.finishDeleting(key)
	if err := p.Runtime.Remove(ctx, v.ID); err != nil && !errors.Is(err, vm.ErrVMNotFound) {
		logger.Errorf(ctx, err, "remove vm %s (%s), keeping pod for investigation", v.ID, reason)
		return
	}
	p.releaseDHCPLeases(ctx, v)
	p.evictPod(ctx, key, pod, reason, message)
}

func (p *Provider) evictGoneIncarnation(ctx context.Context, key string, pod *corev1.Pod, v *vm.VM, reason, message string) {
	if !p.claimDeletingIncarnation(key, pod.UID, v.ID) {
		log.WithFunc("Provider.evictGoneIncarnation").Infof(ctx, "vm %s tracking changed before %s eviction of pod %s/%s, skipping",
			v.ID, reason, pod.Namespace, pod.Name)
		return
	}
	defer p.finishDeleting(key)
	p.releaseDHCPLeases(ctx, v)
	p.evictPod(ctx, key, pod, reason, message)
}

// inspectWithRetry returns on a definitive result (success or ErrVMNotFound) and retries other errors with a growing delay.
func (p *Provider) inspectWithRetry(ctx context.Context, vmID string) (*vm.VM, error) {
	base := cmp.Or(p.inlineInspectBaseDelay, defaultInlineInspectBaseDelay)
	var lastErr error
	for i := range inlineInspectAttempts {
		v, err := p.Runtime.Inspect(ctx, vmID)
		if err == nil || errors.Is(err, vm.ErrVMNotFound) {
			return v, err
		}
		lastErr = err
		if i == inlineInspectAttempts-1 {
			break
		}
		if !commonk8s.SleepCtx(ctx, time.Duration(i+1)*base) {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// scheduleDeferredRecheck re-inspects an inconclusive VM in the background; the ctx check and recheckWG.Go run under p.mu so Close's Wait sees every goroutine.
func (p *Provider) scheduleDeferredRecheck(vmID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lifecycleCtx.Err() != nil {
		return
	}
	if _, running := p.pendingRecheck[vmID]; running {
		return
	}
	p.pendingRecheck[vmID] = struct{}{}
	p.recheckWG.Go(func() { p.runDeferredRecheck(p.lifecycleCtx, vmID) })
}

// runDeferredRecheck loops until the VM resolves or the pod stops being tracked.
func (p *Provider) runDeferredRecheck(ctx context.Context, vmID string) {
	logger := log.WithFunc("Provider.runDeferredRecheck")
	defer func() {
		p.mu.Lock()
		delete(p.pendingRecheck, vmID)
		p.mu.Unlock()
	}()

	delay, maxDelay, budget := p.recheckBackoff()
	deadline := time.Now().Add(budget)
	for {
		if !commonk8s.SleepCtx(ctx, delay) {
			return
		}
		key, pod, tracked := p.podForVMMatch(vmID, "")
		if key == "" || pod == nil {
			return
		}
		v, err := p.Runtime.Inspect(ctx, vmID)
		switch {
		case errors.Is(err, vm.ErrVMNotFound):
			logger.Infof(ctx, "deferred recheck: vm %s confirmed gone, evicting pod %s/%s",
				vmID, pod.Namespace, pod.Name)
			p.evictGoneIncarnation(ctx, key, pod, tracked, "VMGone", "vm no longer exists")
			return
		case err != nil:
			if time.Now().After(deadline) {
				logger.Warnf(ctx, "deferred recheck: vm %s inspect unresolved after %s, removing before eviction of pod %s/%s",
					vmID, budget, pod.Namespace, pod.Name)
				p.removeThenEvict(ctx, tracked, key, pod, "VMInspectTimeout", "vm inspect did not resolve within budget")
				return
			}
			delay = min(delay*2, maxDelay)
		case v.State != vm.StateRunning:
			// a synthetic event keeps the restart-cooldown bookkeeping in one place
			logger.Infof(ctx, "deferred recheck: vm %s non-running (state=%s), replaying event",
				vmID, v.State)
			p.handleVMGone(ctx, v)
			return
		default:
			return
		}
	}
}

func (p *Provider) recheckBackoff() (delay, maxDelay, budget time.Duration) {
	return cmp.Or(p.deferredRecheckInitialDelay, defaultDeferredRecheckInitialDelay),
		cmp.Or(p.deferredRecheckMaxDelay, defaultDeferredRecheckMaxDelay),
		cmp.Or(p.deferredRecheckBudget, defaultDeferredRecheckBudget)
}

// podForVMMatch finds the tracked pod by VM id, or by name when name is set (events can be sparse).
func (p *Provider) podForVMMatch(id, name string) (string, *corev1.Pod, *vm.VM) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for key, tracked := range p.vmsByPod {
		// a name-colliding CH event must match the CH record, never a cocoon-macos guest
		if isMacosVM(tracked) {
			continue
		}
		pod := p.pods[key]
		if pod != nil && (tracked.ID == id || (name != "" && tracked.Name != "" && tracked.Name == name)) {
			return key, pod.DeepCopy(), tracked
		}
	}
	return "", nil, nil
}

// evictPod clears the in-memory tables only once the apiserver delete succeeds or another incarnation owns the name, so a live pod never loses its provider record.
func (p *Provider) evictPod(ctx context.Context, key string, pod *corev1.Pod, reason, message string) {
	logger := log.WithFunc("Provider.evictPod")

	err := p.deletePodWithRetry(ctx, pod)
	if apierrors.IsConflict(err) {
		logger.Infof(ctx, "pod %s/%s was replaced before eviction, dropping superseded incarnation", pod.Namespace, pod.Name)
		p.untrackIncarnation(key, pod.UID)
		return
	}
	if err != nil {
		logger.Errorf(ctx, err, "delete pod %s/%s failed after retries, keeping state for retry",
			pod.Namespace, pod.Name)
		metrics.PodEvictFailureTotal.Inc()
		return
	}

	p.untrackIncarnation(key, pod.UID)

	pod.Status.Phase = corev1.PodFailed
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
	p.notify(pod)
}

// deletePodWithRetry treats IsNotFound as success so repeated evicts are idempotent.
func (p *Provider) deletePodWithRetry(ctx context.Context, pod *corev1.Pod) error {
	opts := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}}
	var lastErr error
	for i := range evictDeleteAttempts {
		err := p.Clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, opts)
		if err == nil || apierrors.IsNotFound(err) {
			return nil
		}
		if apierrors.IsConflict(err) {
			return err
		}
		lastErr = err
		if i == evictDeleteAttempts-1 {
			break
		}
		if !commonk8s.SleepCtx(ctx, time.Duration(i+1)*evictDeleteBaseDelay) {
			return ctx.Err()
		}
	}
	return lastErr
}

// patchIncarnationAnnotations carries metadata.uid so the apiserver rejects the patch once another incarnation owns the name.
func (p *Provider) patchIncarnationAnnotations(ctx context.Context, namespace, name string, uid types.UID, annos map[string]any) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": uid, "annotations": annos},
	})
	if err != nil {
		return fmt.Errorf("marshal annotations %s/%s: %w", namespace, name, err)
	}
	return p.patchPod(ctx, namespace, name, patch)
}

func (p *Provider) patchPod(ctx context.Context, namespace, name string, patch []byte) error {
	if p.Clientset == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := p.Clientset.CoreV1().Pods(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch annotations %s/%s: %w", namespace, name, err)
	}
	return nil
}

// clearRuntimeAnnotations drops VMID, IP and the post-clone marker in memory (under p.mu) and on the apiserver.
func (p *Provider) clearRuntimeAnnotations(ctx context.Context, pod *corev1.Pod) error {
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	if p.supersededLocked(key, pod.UID) {
		p.mu.Unlock()
		return nil
	}
	delete(pod.Annotations, meta.AnnotationVMID)
	delete(pod.Annotations, meta.AnnotationIP)
	delete(pod.Annotations, annotationPostCloneState)
	p.mu.Unlock()
	return p.patchIncarnationAnnotations(ctx, pod.Namespace, pod.Name, pod.UID, map[string]any{
		meta.AnnotationVMID:      nil,
		meta.AnnotationIP:        nil,
		annotationPostCloneState: nil,
	})
}

// buildOnUpdate returns the readiness-transition callback, the only way status reaches the kubelet under the async contract.
func (p *Provider) buildOnUpdate(namespace, name string) probes.OnUpdate {
	return func(ctx context.Context) {
		pod, err := p.GetPod(ctx, namespace, name)
		if err != nil {
			log.WithFunc("Provider.buildOnUpdate").
				Errorf(ctx, err, "pod %s/%s lookup failed, skipping notify", namespace, name)
			return
		}
		if v := p.vmForPod(namespace, name); v != nil && !p.reconcileRuntimeEndpoints(ctx, pod, v.IP) {
			return
		}
		p.refreshAndNotify(ctx, pod)
	}
}

func errDeleteInFlight(pod *corev1.Pod) error {
	return fmt.Errorf("delete operation still in flight for pod %s/%s", pod.Namespace, pod.Name)
}

func patchSuperseded(err error) bool {
	return nameTaken(err) || apierrors.IsNotFound(err)
}

func nameTaken(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsConflict(err)
}

// patchWithRetry retries fn lifecyclePatchAttempts times; a canceled ctx returns nil since nothing is left to log against.
func patchWithRetry(ctx context.Context, fn func() error) error {
	var lastErr error
	for i := range lifecyclePatchAttempts {
		if lastErr = fn(); lastErr == nil {
			return nil
		}
		if patchSuperseded(lastErr) {
			return lastErr
		}
		if i == lifecyclePatchAttempts-1 {
			break
		}
		if !commonk8s.SleepCtx(ctx, lifecyclePatchInterval) {
			return nil
		}
	}
	return lastErr
}

// fanOut runs f over items with bounded concurrency; f logs its own failures.
func fanOut[T any](limit int, items []T, f func(T)) {
	var g errgroup.Group
	g.SetLimit(limit)
	for _, item := range items {
		g.Go(func() error { f(item); return nil })
	}
	_ = g.Wait()
}
