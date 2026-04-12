// Package probes runs the liveness and readiness checks
// virtual-kubelet expects vk-cocoon to maintain for managed pods.
//
// virtual-kubelet treats vk-cocoon as an async provider (because it
// implements NotifyPods), which means the framework never re-polls
// GetPodStatus on its own — the only pod-status updates Kubernetes
// sees are the ones vk-cocoon actively pushes through the notify
// callback. That makes a real per-pod probe loop load-bearing: if
// readiness or the resolved guest IP change asynchronously after
// CreatePod returns, they are invisible to Kubernetes unless this
// package re-triggers a notify.
//
// Manager therefore owns two things:
//   - a lock-protected map of the latest Result per pod, read by
//     GetPodStatus on its hot path, and
//   - a set of per-pod "agents" — one goroutine each — that run a
//     caller-supplied Probe function on a ticker, record the result,
//     and invoke an onUpdate callback whenever the readiness bit
//     flips so the provider can push a fresh PodStatus to v-k.
package probes

import (
	"context"
	"maps"
	"sync"
	"time"
)

const (
	// defaultInitialInterval is the gap between probes during a
	// pod's cold-start window. Tuned so Windows agents — which
	// take roughly 30-90s to finish the firmware handoff, boot,
	// and run DHCP — transition to Ready within a couple of ticks
	// of the guest actually coming up.
	defaultInitialInterval = 2 * time.Second
	// defaultSteadyInterval is the gap between probes once a pod
	// has reached Ready. Liveness monitoring only; slower to keep
	// the pinger's syscall load negligible on large hosts.
	defaultSteadyInterval = 15 * time.Second
	// defaultFailureThreshold is the number of consecutive failed
	// probes after Ready that flip a pod back to unreachable.
	defaultFailureThreshold = 5
)

// Probe is the per-tick health probe supplied by the caller. It
// returns (ready, message); the message is recorded in Result for
// diagnostics and surfaces through GetPodStatus when ready is false.
type Probe func(ctx context.Context) (ready bool, message string)

// Result is the most recent outcome of a probe run.
type Result struct {
	Ready    bool
	Live     bool
	LastSeen time.Time
	Message  string
}

// agent is the per-pod probe loop. One goroutine owns each agent
// and exits when ctx is canceled by Forget (or by the manager
// shutting down).
type agent struct {
	cancel context.CancelFunc
}

// Manager keeps a tiny in-memory map of probe results keyed by
// (namespace, podName). Hot-path lookups (e.g. GetPodStatus) read
// from here without re-running the probe.
//
// agentRoot is the root context every per-pod agent derives from;
// Close cancels it to tear every goroutine down at shutdown.
//
// agents[key] is only written by Start, so readers can compare
// entries by pointer identity to detect a racing replace — that
// invariant is what the race-guard in Start leans on.
type Manager struct {
	agentRoot       context.Context
	agentRootCancel context.CancelFunc

	mu      sync.RWMutex
	results map[string]Result
	agents  map[string]*agent
}

// NewManager constructs an empty Manager. ctx is the root context
// every per-pod agent derives from; Close (or canceling ctx)
// tears every running agent down.
func NewManager(ctx context.Context) *Manager {
	agentCtx, cancel := context.WithCancel(ctx)
	return &Manager{
		agentRoot:       agentCtx,
		agentRootCancel: cancel,
		results:         map[string]Result{},
		agents:          map[string]*agent{},
	}
}

// Close cancels every per-pod agent goroutine. Called once at
// provider shutdown.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.agentRootCancel()
}

// Set stores a probe result for the given pod key.
func (m *Manager) Set(key string, r Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.LastSeen = time.Now().UTC()
	m.results[key] = r
}

// Get returns the latest probe result for the given pod key, or
// zero-value when none has been recorded yet.
func (m *Manager) Get(key string) Result {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.results[key]
}

// Forget drops the entry for a pod, called from DeletePod so the
// map does not grow without bound. It also cancels the associated
// agent goroutine if one is running.
func (m *Manager) Forget(key string) {
	m.mu.Lock()
	ag := m.agents[key]
	delete(m.agents, key)
	delete(m.results, key)
	m.mu.Unlock()
	if ag != nil {
		ag.cancel()
	}
}

// Snapshot copies the entire result map for diagnostics. Used by
// the metrics exporter.
func (m *Manager) Snapshot(_ context.Context) map[string]Result {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]Result, len(m.results))
	maps.Copy(out, m.results)
	return out
}

// OnUpdate is the transition callback the probe agent invokes when
// readiness flips. It receives the per-agent context so the caller
// can propagate cancellation into whatever refresh/notify work it
// performs, instead of conjuring a fresh context.Background().
type OnUpdate func(ctx context.Context)

// Start launches (or replaces) a per-pod probe agent for key. The
// caller-supplied Probe is run once synchronously before Start
// returns so the very first CreatePod/refreshStatus/notify pass
// already reflects the initial reachability decision; subsequent
// probes run on a goroutine until Forget is called or the manager
// closes.
//
// onUpdate is invoked every time the readiness bit flips. The
// provider supplies a closure that re-reads the VM state, rebuilds
// the PodStatus, and calls the virtual-kubelet notify hook — that
// callback is the ONLY way Kubernetes sees status changes under
// the async provider contract.
func (m *Manager) Start(key string, probe Probe, onUpdate OnUpdate) {
	if m == nil || probe == nil {
		return
	}

	// Cancel any previous agent for this key so Start can act as an
	// idempotent "ensure a probe loop is running" helper.
	m.mu.Lock()
	if prev, ok := m.agents[key]; ok {
		prev.cancel()
		delete(m.agents, key)
	}
	ctx, cancel := context.WithCancel(m.agentRoot)
	ag := &agent{cancel: cancel}
	m.agents[key] = ag
	m.mu.Unlock()

	// First probe runs synchronously. The CreatePod caller is about
	// to refreshStatus + notify unconditionally, so whatever this
	// probe returns is what Kubernetes will see in the initial push.
	ready, message := runProbe(ctx, probe)

	// Guard against a racing Forget(key) that canceled us between
	// the registration above and the sync probe finishing: if the
	// agent entry is no longer ours, the caller already gave up on
	// this pod, so do not write the stale result back into the map
	// or spawn a goroutine that would just exit on the first tick.
	m.mu.Lock()
	if current, ok := m.agents[key]; !ok || current != ag {
		m.mu.Unlock()
		cancel()
		return
	}
	m.results[key] = Result{
		Ready:    ready,
		Live:     ready,
		Message:  message,
		LastSeen: time.Now().UTC(),
	}
	m.mu.Unlock()

	go m.run(ctx, key, probe, onUpdate, ready)
}

// run is the per-pod agent loop. It schedules probes on a ticker
// that starts fast (defaultInitialInterval) and slows to steady
// state once the pod is Ready, and invokes onUpdate on every
// readiness transition.
func (m *Manager) run(ctx context.Context, key string, probe Probe, onUpdate OnUpdate, lastReady bool) {
	interval := defaultInitialInterval
	if lastReady {
		interval = defaultSteadyInterval
	}
	failures := 0

	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		ready, message := runProbe(ctx, probe)
		m.applyResult(key, ready, message)

		switch {
		case ready && !lastReady:
			// Transition to Ready: push immediately and slow to steady.
			lastReady = true
			failures = 0
			interval = defaultSteadyInterval
			if onUpdate != nil {
				onUpdate(ctx)
			}
		case ready:
			failures = 0
		case lastReady:
			// Steady-state flap: only flip back to unreachable
			// after the failure budget is exhausted, so a single
			// dropped ICMP reply does not demote a healthy pod.
			failures++
			if failures >= defaultFailureThreshold {
				lastReady = false
				interval = defaultInitialInterval
				if onUpdate != nil {
					onUpdate(ctx)
				}
			}
		}
	}
}

// runProbe runs probe under a bounded timeout derived from ctx so
// one slow ICMP reply cannot stall the whole agent loop.
func runProbe(ctx context.Context, probe Probe) (bool, string) {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return probe(probeCtx)
}

// applyResult writes one probe outcome into the result map. Ready
// implies Live; a false ready with a blank message still records
// "not ready" rather than blowing away the previous message.
func (m *Manager) applyResult(key string, ready bool, message string) {
	m.Set(key, Result{
		Ready:   ready,
		Live:    ready,
		Message: message,
	})
}
