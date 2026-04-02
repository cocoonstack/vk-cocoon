package provider

import (
	"context"
	"fmt"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestSnapshotManagerSaveSnapshotRemovesExistingBeforeSave(t *testing.T) {
	p := newTestProvider()
	var calls []string
	p.cocoonExecFn = func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, fmt.Sprint(args))
		return "ok", nil
	}

	out, err := p.snapshotManager().saveSnapshot(context.Background(), "snap-1", "vm-123")
	if err != nil {
		t.Fatalf("saveSnapshot: %v", err)
	}
	if out != "ok" {
		t.Fatalf("saveSnapshot output = %q, want ok", out)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[0] != "[snapshot rm snap-1]" {
		t.Fatalf("first call = %q, want snapshot rm", calls[0])
	}
	if calls[1] != "[snapshot save --name snap-1 vm-123]" {
		t.Fatalf("second call = %q, want snapshot save", calls[1])
	}
}

func TestSnapshotManagerRecordLookupAndClearSuspendedSnapshot(t *testing.T) {
	p := newTestProvider()
	p.lookupSuspendedSnapshotFn = nil
	p.kubeClient = fake.NewSimpleClientset()
	mgr := p.snapshotManager()

	pod := newTestPod("app-0", nil)
	mgr.recordSuspendedSnapshot(context.Background(), pod, "vk-testns-app-0", "epoch.local/vk-testns-app-0-suspend")

	if got := mgr.lookupSuspendedSnapshot(context.Background(), pod.Namespace, "vk-testns-app-0"); got != "epoch.local/vk-testns-app-0-suspend" {
		t.Fatalf("lookupSuspendedSnapshot = %q, want epoch.local/vk-testns-app-0-suspend", got)
	}

	mgr.clearSuspendedSnapshot(context.Background(), pod.Namespace, "vk-testns-app-0")
	if got := mgr.lookupSuspendedSnapshot(context.Background(), pod.Namespace, "vk-testns-app-0"); got != "" {
		t.Fatalf("lookupSuspendedSnapshot after clear = %q, want empty", got)
	}
}
