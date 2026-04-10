// Package probes runs the liveness and readiness checks
// virtual-kubelet expects vk-cocoon to maintain for managed pods.
// The probe loop is event-driven (kicked by the per-pod creation
// hook) rather than a global ticker so dormant pods do not consume
// CPU.
package probes

import (
	"context"
	"maps"
	"sync"
	"time"
)

// Result is the most recent outcome of a probe run.
type Result struct {
	Ready    bool
	Live     bool
	LastSeen time.Time
	Message  string
}

// Manager keeps a tiny in-memory map of probe results keyed by
// (namespace, podName). Hot-path lookups (e.g. GetPodStatus) read
// from here without re-running the probe.
type Manager struct {
	mu      sync.RWMutex
	results map[string]Result
}

// NewManager constructs an empty Manager.
func NewManager() *Manager {
	return &Manager{results: map[string]Result{}}
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
// map does not grow without bound.
func (m *Manager) Forget(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.results, key)
}

// MarkReady is the convenience setter the create / status code
// uses when the VM has reached a usable state.
func (m *Manager) MarkReady(key string) {
	m.Set(key, Result{Ready: true, Live: true})
}

// MarkUnreachable records a pod as not ready (e.g. when the VM
// disappears or stops responding to inspect).
func (m *Manager) MarkUnreachable(key, message string) {
	m.Set(key, Result{Ready: false, Live: false, Message: message})
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
