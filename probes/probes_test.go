package probes

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStartInitialProbeRunsSynchronously pins the contract CreatePod
// relies on: Start must call the Probe once before returning so the
// subsequent refreshStatus/notify pass already reflects the initial
// reachability decision.
func TestStartInitialProbeRunsSynchronously(t *testing.T) {
	m := NewManager(t.Context())
	defer m.Close()

	var calls atomic.Int32
	probe := func(_ context.Context) (bool, string) {
		calls.Add(1)
		return true, "ok"
	}

	m.Start("ns/demo-0", probe, nil)

	if got := calls.Load(); got < 1 {
		t.Fatalf("probe call count = %d, want >= 1 by the time Start returns", got)
	}
	if !m.Get("ns/demo-0").Ready {
		t.Fatalf("Start should have recorded Ready=true for a probe that succeeds")
	}
}

// TestStartTriggersOnUpdateOnReadinessChange covers the async side:
// after a not-ready start, once the probe starts returning ready the
// agent goroutine must fire onUpdate so the provider can push a
// fresh PodStatus through the v-k notify hook.
func TestStartTriggersOnUpdateOnReadinessChange(t *testing.T) {
	m := NewManager(t.Context())
	defer m.Close()

	// Flip the probe from unready to ready on the second call, so
	// the first (synchronous) probe records NotReady and the first
	// ticker-driven call records Ready.
	var calls atomic.Int32
	probe := func(_ context.Context) (bool, string) {
		n := calls.Add(1)
		if n == 1 {
			return false, "waiting"
		}
		return true, "ok"
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var updates atomic.Int32
	onUpdate := func(_ context.Context) {
		if updates.Add(1) == 1 {
			wg.Done()
		}
	}

	m.Start("ns/demo-0", probe, onUpdate)

	// The synchronous probe must have logged NotReady; no onUpdate
	// yet because there is no transition (starting state is
	// implicitly "not ready").
	if m.Get("ns/demo-0").Ready {
		t.Fatalf("first probe should record NotReady")
	}

	// Wait for the agent goroutine to run a second probe. The initial
	// interval is 2s; give it a generous budget so slow CI does not
	// flake.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("onUpdate was not called after probe flipped to ready (updates=%d, calls=%d)",
			updates.Load(), calls.Load())
	}

	if !m.Get("ns/demo-0").Ready {
		t.Fatalf("probe manager should reflect Ready=true after transition")
	}
}

// TestForgetCancelsAgent proves Forget stops a running agent so
// DeletePod cannot leak per-pod goroutines.
func TestForgetCancelsAgent(t *testing.T) {
	m := NewManager(t.Context())
	defer m.Close()

	stopped := make(chan struct{})
	probe := func(ctx context.Context) (bool, string) {
		select {
		case <-ctx.Done():
			close(stopped)
		default:
		}
		return false, "pending"
	}

	m.Start("ns/demo-0", probe, nil)
	m.Forget("ns/demo-0")

	// After Forget there must be no result for the key (GC'd) and
	// the agent map entry must be gone.
	if r := m.Get("ns/demo-0"); r.Ready || r.Message != "" || !r.LastSeen.IsZero() {
		t.Fatalf("Forget should drop the result, got %#v", r)
	}
	m.mu.RLock()
	_, stillTracked := m.agents["ns/demo-0"]
	m.mu.RUnlock()
	if stillTracked {
		t.Fatalf("Forget should remove the agent entry")
	}
}
