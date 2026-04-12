package probes

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

func TestStartTriggersOnUpdateOnReadinessChange(t *testing.T) {
	m := NewManager(t.Context())
	defer m.Close()

	// Flip from unready to ready on the second call.
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

	// First probe should be NotReady; no onUpdate yet (no transition).
	if m.Get("ns/demo-0").Ready {
		t.Fatalf("first probe should record NotReady")
	}

	// Wait for the agent to run a second probe (generous timeout for CI).
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

	// Result and agent entry should be gone.
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
