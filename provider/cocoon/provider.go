package cocoon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
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
	// restartCooldown prevents tight restart loops when a VM keeps crashing.
	restartCooldown = 30 * time.Second

	// initialStatusPushDelay waits out the kubelet pod-informer's knownPods
	// population window so the first post-restart status push isn't dropped.
	initialStatusPushDelay  = 10 * time.Second
	statusReconcileInterval = 30 * time.Second

	// containerName is the synthetic container name used in pod status and metrics.
	containerName = "agent"

	// evictDeleteAttempts / evictDeleteBaseDelay bound evictPod's K8s-side
	// retry; kept small so a flaky apiserver can't stall the serialized event loop.
	evictDeleteAttempts  = 2
	evictDeleteBaseDelay = 200 * time.Millisecond

	// inlineInspectAttempts bounds handleVMGone's synchronous retry; beyond
	// a single CLI hiccup the deferred recheck takes over.
	inlineInspectAttempts = 2

	// Default tunables for the recheck path. Overridable via Provider
	// fields so tests can shrink them without racing on package globals.
	defaultInlineInspectBaseDelay      = 200 * time.Millisecond
	defaultDeferredRecheckInitialDelay = 1 * time.Second
	defaultDeferredRecheckMaxDelay     = 30 * time.Second

	// defaultDeferredRecheckBudget caps one recheck loop; on timeout the pod
	// is evicted (VMInspectTimeout) so a broken cocoon can't leave it tracked forever.
	defaultDeferredRecheckBudget = 30 * time.Minute
)

// Provider maps Kubernetes pods to cocoon MicroVMs.
type Provider struct {
	NodeName string

	OrphanPolicy provider.OrphanPolicy
	RestoreMode  vm.RestoreMode

	Clientset   kubernetes.Interface
	Runtime     vm.Runtime
	Puller      *snapshots.Puller
	Pusher      *snapshots.Pusher
	Registry    oci.Registry
	LeaseParser *network.LeaseParser
	Pinger      network.Pinger
	GuestSAC    guest.Dialer
	Probes      *probes.Manager
	Recorder    record.EventRecorder

	startTime time.Time
	//nolint:containedctx // deferred recheck must outlive the watcher ctx (which cycles on event-stream reconnect) and be cancelable only by Close
	lifecycleCtx   context.Context
	lifecycleStop  context.CancelFunc // canceled from Close to stop deferred goroutines
	mu             sync.RWMutex
	pods           map[string]*corev1.Pod
	vmsByPod       map[string]*vm.VM
	vmsByName      map[string]*vm.VM
	lastRestart    map[string]time.Time // key=vmID, cooldown for restart loops
	pendingRecheck map[string]struct{}  // key=vmID, dedup for deferred recheck goroutines
	resumedOps     map[string]struct{}  // key=pod, full ops resumed by dispatchOwedWork; UpdatePod backs off
	recheckWG      sync.WaitGroup       // tracks deferred recheck goroutines so Close can await them
	bgWG           sync.WaitGroup       // tracks per-pod async goroutines (post-clone exec, static-IP) so Close can await them
	forkSnapshotSF singleflight.Group   // dedups concurrent fork-base snapshot creation (self-synchronized)
	snapshotPullSF singleflight.Group   // dedups concurrent registry pulls of one local snapshot name (self-synchronized)
	runImageSF     singleflight.Group   // dedups concurrent base-image materialization of one ref (self-synchronized)
	notifyHook     func(*corev1.Pod)
	// Source of truth for lifecycle annotations (decoupled from p.pods).
	lifecycleIntent  map[string]meta.LifecycleStatus
	lifecycleFlushed map[string]string

	// Recheck tunables. Zero values fall back to the defaultXxx
	// constants, so production code never sets them; tests shrink them
	// before exercising handleVMGone.
	inlineInspectBaseDelay      time.Duration
	deferredRecheckInitialDelay time.Duration
	deferredRecheckMaxDelay     time.Duration
	deferredRecheckBudget       time.Duration

	// dropNIC wake tunables; defaults live in update.go.
	wakeFreshIPBudget   time.Duration
	wakeFreshIPInterval time.Duration
	wakeRenewNudgeDelay time.Duration
}

// NewProvider constructs a Provider with empty tables; background work stops
// when ctx is canceled or Close is called. Default Pinger is NopPinger so
// tests degrade gracefully.
func NewProvider(ctx context.Context) *Provider {
	lifecycleCtx, lifecycleStop := context.WithCancel(ctx)
	return &Provider{
		startTime:        time.Now(),
		lifecycleCtx:     lifecycleCtx,
		lifecycleStop:    lifecycleStop,
		OrphanPolicy:     provider.OrphanDestroy,
		RestoreMode:      vm.RestoreOnDemand,
		Pinger:           network.NopPinger{},
		pods:             map[string]*corev1.Pod{},
		vmsByPod:         map[string]*vm.VM{},
		vmsByName:        map[string]*vm.VM{},
		lastRestart:      map[string]time.Time{},
		pendingRecheck:   map[string]struct{}{},
		resumedOps:       map[string]struct{}{},
		lifecycleIntent:  map[string]meta.LifecycleStatus{},
		lifecycleFlushed: map[string]string{},
	}
}

// Close cancels background goroutines and waits for them. The lifecycle
// cancel runs under p.mu so spawn paths cannot Add to a waitgroup after
// Wait has returned.
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
	return slices.Collect(maps.Values(p.pods)), nil
}

// NotifyPods stores the kubelet's pod-status callback and schedules a
// deferred initial push so adopted pods (post-restart) leave Pending.
// virtual-kubelet calls NotifyPods before WaitForCacheSync; pushing
// synchronously hits enqueuePodStatusUpdate's empty knownPods and is
// dropped after a ~3s poll budget. The reconciler waits past that window.
func (p *Provider) NotifyPods(_ context.Context, notifier func(*corev1.Pod)) {
	p.mu.Lock()
	p.notifyHook = notifier
	p.mu.Unlock()
	p.goBackground(func() {
		p.runStatusReconciler(p.lifecycleCtx)
	})
}

// StartVMWatcher launches a background goroutine that subscribes to cocoon's
// VM event stream and reacts to VM state changes in near-real-time.
func (p *Provider) StartVMWatcher(ctx context.Context) {
	go p.vmWatchLoop(ctx)
}

// goBackground spawns f tracked by p.bgWG, taking p.mu so Add cannot
// race Close's lifecycleStop+Wait. A bgWG.Go after Close's Wait would
// trip the sync.WaitGroup add-after-wait misuse.
func (p *Provider) goBackground(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lifecycleCtx.Err() != nil {
		return
	}
	p.bgWG.Go(f)
}

// runStatusReconciler repairs status notifications dropped during startup or
// an apiserver outage without touching the VM lifecycle.
func (p *Provider) runStatusReconciler(ctx context.Context) {
	if !commonk8s.SleepCtx(ctx, initialStatusPushDelay) {
		return
	}
	p.reconcilePodStatuses(ctx)

	ticker := time.NewTicker(statusReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.reconcilePodStatuses(ctx)
		}
	}
}

func (p *Provider) reconcilePodStatuses(ctx context.Context) {
	p.mu.RLock()
	pods := slices.Collect(maps.Values(p.pods))
	p.mu.RUnlock()
	if len(pods) == 0 {
		return
	}
	logger := log.WithFunc("Provider.reconcilePodStatuses")
	for _, pod := range pods {
		current, err := p.Clientset.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			logger.Errorf(ctx, err, "get pod %s/%s for status reconciliation", pod.Namespace, pod.Name)
			continue
		}
		status, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name)
		if err != nil {
			logger.Errorf(ctx, err, "derive pod %s/%s status", pod.Namespace, pod.Name)
			continue
		}
		if podStatusMatches(current.Status, *status) {
			continue
		}
		current.Status = *status
		logger.Infof(ctx, "republishing drifted status for pod %s/%s", pod.Namespace, pod.Name)
		p.notify(current)
	}
}

func (p *Provider) notify(pod *corev1.Pod) {
	p.mu.RLock()
	hook := p.notifyHook
	p.mu.RUnlock()
	if hook != nil {
		hook(pod)
	}
}

func (p *Provider) trackPod(pod *corev1.Pod, v *vm.VM) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := meta.PodKey(pod.Namespace, pod.Name)
	// UpdatePod hands us a fresh framework snapshot that can carry a stale
	// lifecycle-state (e.g. creating) captured before vk advanced it out-of-band.
	// Re-assert the authoritative intent, else the framework syncs that stale
	// annotation back to the apiserver and strands a healthy VM at creating.
	if intent, ok := p.lifecycleIntent[key]; ok {
		intent.Apply(pod)
	}
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
	delete(p.vmsByName, v.Name)
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
	key := meta.PodKey(namespace, name)
	p.mu.Lock()
	p.dropVMLocked(key)
	delete(p.pods, key)
	delete(p.lifecycleIntent, key)
	delete(p.lifecycleFlushed, key)
	p.mu.Unlock()
	if p.Probes != nil {
		p.Probes.Forget(key)
	}
}

func (p *Provider) vmForPod(namespace, name string) *vm.VM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.vmsByPod[meta.PodKey(namespace, name)]
}

// setVMIP updates the tracked VM's IP (copy-on-write for concurrency safety).
func (p *Provider) setVMIP(namespace, name, vmID, ip string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := meta.PodKey(namespace, name)
	v, ok := p.vmsByPod[key]
	if !ok || v.ID != vmID {
		return false
	}
	updated := *v
	updated.IP = ip
	p.vmsByPod[key] = &updated
	if updated.Name != "" {
		p.vmsByName[updated.Name] = &updated
	}
	return true
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
	// The lease belongs to v's MAC; if a same-name recreate swapped the tracked
	// VM during the lookup, the IP must not leak onto the successor.
	if !p.setVMIP(namespace, name, v.ID, lease.IP) {
		return ""
	}
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

// probePort reads the annotation under RLock without GetPod's full
// DeepCopy — this runs on every probe tick.
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

// handleVMGone processes a DELETED or MODIFIED(stopped/error) event.
// It double-checks via `cocoon vm inspect` before acting to avoid
// false positives from transient states (e.g. watchdog restart).
//
// - VM gone (inspect fails): delete pod → operator recreates.
// - VM stopped/error: restart via `cocoon vm start` → probe re-pings.
// - VM still running: ignore (false alarm).
func (p *Provider) handleVMGone(ctx context.Context, eventVM *vm.VM) {
	logger := log.WithFunc("Provider.handleVMGone")

	affectedKey, affectedPod, trackedID := p.podForVMMatch(eventVM.ID, eventVM.Name)
	if affectedKey == "" || affectedPod == nil {
		return
	}

	// Hibernate's own Runtime.Remove triggers this event; restarting would race the cleanup.
	if meta.ReadHibernateState(affectedPod) {
		logger.Infof(ctx, "vm %s pod %s/%s is hibernating, skipping VM-gone handler",
			trackedID, affectedPod.Namespace, affectedPod.Name)
		return
	}

	inspected, err := p.inspectWithRetry(ctx, trackedID)
	switch {
	case errors.Is(err, vm.ErrVMNotFound):
		logger.Infof(ctx, "vm %s confirmed gone, deleting pod %s/%s",
			trackedID, affectedPod.Namespace, affectedPod.Name)
		p.evictPod(ctx, affectedKey, affectedPod, "VMGone", "vm no longer exists")

	case err != nil:
		// Still transient after inline retries. Spawn a deferred recheck so
		// a genuinely gone VM does not leave the pod stuck indefinitely —
		// cocoon does not re-emit DELETED, and probes only ping IPs.
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
			p.removeThenEvict(ctx, trackedID, affectedKey, affectedPod, "RestartCooldown", "restart cooldown not elapsed")
			return
		}
		logger.Infof(ctx, "vm %s state=%s, restarting", trackedID, inspected.State)
		if startErr := p.Runtime.Start(ctx, trackedID); startErr != nil {
			logger.Errorf(ctx, startErr, "restart vm %s failed, removing VM and evicting pod", trackedID)
			p.removeThenEvict(ctx, trackedID, affectedKey, affectedPod, "RestartFailed", startErr.Error())
			return
		}
		// Re-inspect to refresh PID and NetworkConfigs for stats collection.
		if fresh, inspectErr := p.Runtime.Inspect(ctx, trackedID); inspectErr == nil {
			p.mu.Lock()
			if old := p.vmsByPod[affectedKey]; old != nil {
				updated := *old
				updated.PID = fresh.PID
				updated.NetworkConfigs = fresh.NetworkConfigs
				p.vmsByPod[affectedKey] = &updated
				if updated.Name != "" {
					p.vmsByName[updated.Name] = &updated
				}
			}
			p.mu.Unlock()
		}
	}
}

// removeThenEvict removes the VM then evicts the pod. On remove failure the
// pod is kept for investigation — evicting would orphan the live VM and
// collide on recreate.
func (p *Provider) removeThenEvict(ctx context.Context, vmID, key string, pod *corev1.Pod, reason, message string) {
	if err := p.Runtime.Remove(ctx, vmID); err != nil {
		log.WithFunc("Provider.removeThenEvict").
			Errorf(ctx, err, "remove vm %s (%s), keeping pod for investigation", vmID, reason)
		return
	}
	p.evictPod(ctx, key, pod, reason, message)
}

// inspectWithRetry calls Runtime.Inspect up to inlineInspectAttempts
// times, returning early on a definitive result (success or
// ErrVMNotFound). Other errors are treated as transient and retried
// with a linearly growing delay.
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

// scheduleDeferredRecheck spawns a background goroutine that keeps
// re-inspecting a VM whose event classification was inconclusive, until
// cocoon returns a definitive answer or the pod is no longer tracked.
// Dedup'd via pendingRecheck so repeated transient events for the same
// VM don't multiply goroutines.
//
// The lifecycle-ctx check and recheckWG.Add happen under p.mu to pair
// with Close's cancel-under-lock, so every goroutine this function
// starts is visible to Close's Wait.
func (p *Provider) scheduleDeferredRecheck(vmID string) {
	p.mu.Lock()
	if p.lifecycleCtx.Err() != nil {
		p.mu.Unlock()
		return
	}
	if _, running := p.pendingRecheck[vmID]; running {
		p.mu.Unlock()
		return
	}
	p.pendingRecheck[vmID] = struct{}{}
	p.recheckWG.Add(1)
	p.mu.Unlock()

	go p.runDeferredRecheck(p.lifecycleCtx, vmID)
}

// runDeferredRecheck is the deferred-recheck loop started by
// scheduleDeferredRecheck. Exits when the VM resolves or the pod
// stops being tracked.
func (p *Provider) runDeferredRecheck(ctx context.Context, vmID string) {
	logger := log.WithFunc("Provider.runDeferredRecheck")
	defer p.recheckWG.Done()
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
		key, pod, _ := p.podForVMMatch(vmID, "")
		if key == "" || pod == nil {
			return
		}
		v, err := p.Runtime.Inspect(ctx, vmID)
		switch {
		case errors.Is(err, vm.ErrVMNotFound):
			logger.Infof(ctx, "deferred recheck: vm %s confirmed gone, evicting pod %s/%s",
				vmID, pod.Namespace, pod.Name)
			p.evictPod(ctx, key, pod, "VMGone", "vm no longer exists")
			return
		case err != nil:
			if time.Now().After(deadline) {
				logger.Warnf(ctx, "deferred recheck: vm %s inspect unresolved after %s, evicting pod %s/%s to avoid stuck state",
					vmID, budget, pod.Namespace, pod.Name)
				p.evictPod(ctx, key, pod, "VMInspectTimeout", "vm inspect did not resolve within budget")
				return
			}
			if delay < maxDelay {
				delay = min(delay*2, maxDelay)
			}
		case v.State != vm.StateRunning:
			// Stopped/error — let the normal event path handle restart.
			// Feeding a synthetic event keeps restart-cooldown bookkeeping
			// in one place instead of duplicating it here.
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

// podForVMMatch returns the pod and tracked-VM ID for a pod that matches
// the given id or (optionally) name. name may be empty to restrict the
// match to id only. Used by handleVMGone (match on id OR name from a
// potentially sparse event) and by runDeferredRecheck (id only).
func (p *Provider) podForVMMatch(id, name string) (string, *corev1.Pod, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for key, tracked := range p.vmsByPod {
		if tracked.ID == id || (name != "" && tracked.Name != "" && tracked.Name == name) {
			return key, p.pods[key], tracked.ID
		}
	}
	return "", nil, ""
}

// evictPod deletes the pod from the API server first, then clears the
// in-memory tables only on success. Keeping K8s as the authoritative
// step means a transient API failure leaves memory intact so the next
// VM event or probe can retry, rather than leaving a live K8s pod with
// no provider record. The pod is always marked PodFailed on success.
func (p *Provider) evictPod(ctx context.Context, key string, pod *corev1.Pod, reason, message string) {
	logger := log.WithFunc("Provider.evictPod")

	if err := p.deletePodWithRetry(ctx, pod); err != nil {
		// Leave in-memory state intact so the next VM event re-enters
		// evictPod instead of stranding the pod half-detached.
		logger.Errorf(ctx, err, "delete pod %s/%s failed after retries, keeping state for retry",
			pod.Namespace, pod.Name)
		metrics.PodEvictFailureTotal.Inc()
		return
	}

	p.mu.Lock()
	p.dropVMLocked(key)
	delete(p.pods, key)
	delete(p.lifecycleIntent, key)
	delete(p.lifecycleFlushed, key)
	p.mu.Unlock()

	if p.Probes != nil {
		p.Probes.Forget(key)
	}

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

// deletePodWithRetry deletes the pod with a short bounded retry.
// IsNotFound is success so repeated evict calls are idempotent.
func (p *Provider) deletePodWithRetry(ctx context.Context, pod *corev1.Pod) error {
	var lastErr error
	for i := range evictDeleteAttempts {
		err := p.Clientset.CoreV1().Pods(pod.Namespace).Delete(
			ctx, pod.Name, metav1.DeleteOptions{},
		)
		if err == nil || apierrors.IsNotFound(err) {
			return nil
		}
		lastErr = err
		if !commonk8s.SleepCtx(ctx, time.Duration(i+1)*evictDeleteBaseDelay) {
			return ctx.Err()
		}
	}
	return lastErr
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
// annotations (under p.mu against GetPod's DeepCopy) and patches the API
// server. Used by hibernate and startup reconcile.
func (p *Provider) clearRuntimeAnnotations(ctx context.Context, pod *corev1.Pod) error {
	p.mu.Lock()
	delete(pod.Annotations, meta.AnnotationVMID)
	delete(pod.Annotations, meta.AnnotationIP)
	p.mu.Unlock()
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
			log.WithFunc("Provider.buildOnUpdate").
				Errorf(ctx, err, "pod %s/%s lookup failed, skipping notify", namespace, name)
			return
		}
		p.refreshStatus(ctx, pod)
		p.notify(pod)
	}
}
