package snapshots

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPushSnapshotWaitsOnTheGateForSpoolPushes(t *testing.T) {
	if err := pushGate.Acquire(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	defer pushGate.Release(1)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := (&Pusher{}).PushSnapshot(ctx, "vm-a", "", "", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a v1 spool push with the gate held returned %v, want the gate wait to expire", err)
	}
}
