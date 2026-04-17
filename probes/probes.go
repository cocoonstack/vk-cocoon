// Package probes runs per-pod probe loops that push readiness updates
// through the v-k notify callback. Under the async provider contract
// this is the only way status changes reach Kubernetes.
package probes

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/cocoonstack/vk-cocoon/metrics"
)

const (
	defaultInitialInterval  = 2 * time.Second
	defaultSteadyInterval   = 5 * time.Second
	defaultFailureThreshold = 3
)

// Probe is the per-tick health check; returns (ready, message).
type Probe func(ctx context.Context) (ready bool, message string)

// Result is the latest probe outcome.
type Result struct {
	Ready    bool
	Live     bool
	LastSeen time.Time
	Message  string
}

// Manager tracks probe results per pod and manages per-pod agent goroutines.
type Manager struct {
	agentRoot       context.Context
	agentRootCancel context.CancelFunc

	mu      sync.RWMutex
	results map[string]Result
	agents  map[string]*agent
}

// agent is a per-pod probe goroutine, canceled by Forget or shutdown.
type agent struct {
	cancel context.CancelFunc
}

// NewManager constructs an empty Manager. Close or canceling ctx tears agents down.
func NewManager(ctx context.Context) *Manager {
	agentCtx, cancel := context.WithCancel(ctx)
	return &Manager{
		agentRoot:       agentCtx,
		agentRootCancel: cancel,
		results:         map[string]Result{},
		agents:          map[string]*agent{},
	}
}

// Close cancels all agent goroutines.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.agentRootCancel()
}

// Set stores a probe result.
func (m *Manager) Set(key string, r Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.LastSeen = time.Now().UTC()
	m.results[key] = r
}

// Get returns the latest probe result, or zero-value if none recorded.
func (m *Manager) Get(key string) Result {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.results[key]
}

// Forget drops the pod entry and cancels its agent.
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

// Snapshot copies the entire result map.
func (m *Manager) Snapshot(_ context.Context) map[string]Result {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]Result, len(m.results))
	maps.Copy(out, m.results)
	return out
}

// OnUpdate is called when readiness flips; receives the per-agent context.
type OnUpdate func(ctx context.Context)

// Start launches (or replaces) a per-pod probe agent. The first probe runs
// synchronously so CreatePod's initial notify reflects reachability.
func (m *Manager) Start(key string, probe Probe, onUpdate OnUpdate) {
	if m == nil || probe == nil {
		return
	}

	// Cancel any previous agent for this key.
	m.mu.Lock()
	if prev, ok := m.agents[key]; ok {
		prev.cancel()
		delete(m.agents, key)
	}
	ctx, cancel := context.WithCancel(m.agentRoot)
	ag := &agent{cancel: cancel}
	m.agents[key] = ag
	m.mu.Unlock()

	// First probe runs synchronously.
	ready, message := runProbe(ctx, probe)

	// Guard against a racing Forget that canceled us during the sync probe.
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

// run is the per-pod agent loop. Starts fast, slows to steady state once Ready.
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
			// Only flip back after the failure budget is exhausted.
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

// runProbe runs probe with a bounded 3s timeout.
func runProbe(ctx context.Context, probe Probe) (bool, string) {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	start := time.Now()
	ok, msg := probe(probeCtx)
	metrics.ProbeDuration.Observe(time.Since(start).Seconds())
	return ok, msg
}

// applyResult writes one probe outcome into the result map.
func (m *Manager) applyResult(key string, ready bool, message string) {
	m.Set(key, Result{
		Ready:   ready,
		Live:    ready,
		Message: message,
	})
}
