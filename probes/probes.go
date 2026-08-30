// Package probes runs per-pod probe loops that push readiness updates through the v-k notify callback.
package probes

import (
	"context"
	"sync"
	"time"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/vk-cocoon/metrics"
)

// Pre-Ready polling starts at 100ms so a fast guest flips Ready in ~100ms instead of paying a fixed wait; the cap bounds slow guests.
const (
	defaultInitialInterval    = 100 * time.Millisecond
	defaultInitialBackoffMax  = 1 * time.Second
	defaultInitialBackoffStep = 1.5

	defaultSteadyInterval   = 5 * time.Second
	defaultFailureThreshold = 3
)

// Probe is the per-tick health check; returns (ready, message).
type Probe func(ctx context.Context) (ready bool, message string)

// Result is the latest probe outcome.
type Result struct {
	Ready    bool
	LastSeen time.Time
	Message  string
}

// OnUpdate is called when readiness flips; receives the per-agent context.
type OnUpdate func(ctx context.Context)

// agent is a per-pod probe goroutine, canceled by Forget or shutdown.
type agent struct {
	cancel context.CancelFunc
}

// Manager tracks probe results per pod and manages per-pod agent goroutines.
type Manager struct {
	agentRoot       context.Context
	agentRootCancel context.CancelFunc

	mu      sync.RWMutex
	results map[string]Result
	agents  map[string]*agent
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

// Start launches (or replaces) a per-pod probe agent; the first probe runs synchronously so CreatePod's initial notify reflects reachability.
func (m *Manager) Start(key string, probe Probe, onUpdate OnUpdate) {
	m.mu.Lock()
	if prev, ok := m.agents[key]; ok {
		prev.cancel()
		delete(m.agents, key)
	}
	ctx, cancel := context.WithCancel(m.agentRoot)
	ag := &agent{cancel: cancel}
	m.agents[key] = ag
	m.mu.Unlock()

	ready, message := runProbe(ctx, probe)

	// guard against a racing Forget that canceled us during the sync probe.
	m.mu.Lock()
	if current, ok := m.agents[key]; !ok || current != ag {
		m.mu.Unlock()
		cancel()
		return
	}
	m.results[key] = Result{
		Ready:    ready,
		Message:  message,
		LastSeen: time.Now().UTC(),
	}
	m.mu.Unlock()

	go m.run(ctx, key, probe, onUpdate, ready)
}

func (m *Manager) run(ctx context.Context, key string, probe Probe, onUpdate OnUpdate, lastReady bool) {
	interval := defaultInitialInterval
	if lastReady {
		interval = defaultSteadyInterval
	}
	failures := 0

	for {
		if !commonk8s.SleepCtx(ctx, interval) {
			return
		}

		ready, message := runProbe(ctx, probe)
		m.Set(key, Result{Ready: ready, Message: message})

		switch {
		case ready && !lastReady:
			lastReady = true
			failures = 0
			interval = defaultSteadyInterval
			if onUpdate != nil {
				onUpdate(ctx)
			}
		case ready:
			failures = 0
		case !lastReady:
			interval = nextInitialInterval(interval)
		case lastReady:
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

// nextInitialInterval grows the pre-Ready poll interval exponentially until it hits the cap.
func nextInitialInterval(d time.Duration) time.Duration {
	return min(time.Duration(float64(d)*defaultInitialBackoffStep), defaultInitialBackoffMax)
}

func runProbe(ctx context.Context, probe Probe) (bool, string) {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	start := time.Now()
	ok, msg := probe(probeCtx)
	metrics.ProbeDuration.Observe(time.Since(start).Seconds())
	return ok, msg
}
