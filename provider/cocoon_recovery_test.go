package provider

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newTestProvider() *CocoonProvider {
	return &CocoonProvider{
		cocoonBin:                 "/definitely-missing-cocoon",
		sshPassword:               "test-password",
		pods:                      make(map[string]*corev1.Pod),
		vms:                       make(map[string]*CocoonVM),
		injectHashes:              make(map[string]string),
		probeStates:               make(map[string]*probeResult),
		pullers:                   make(map[string]*EpochPuller),
		lookupSuspendedSnapshotFn: func(_ context.Context, _, _ string) string { return "" },
	}
}

func newTestPod(name string, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "testns",
			Name:        name,
			Annotations: annotations,
		},
	}
}

func TestCreatePodRecoversExistingManagedVMByID(t *testing.T) {
	p := newTestProvider()
	p.discoverVMByIDFn = func(_ context.Context, vmID string) *CocoonVM {
		if vmID != "vm-123" {
			t.Fatalf("discoverVMByID got %q, want vm-123", vmID)
		}
		return &CocoonVM{
			vmID:   vmID,
			vmName: "vk-testns-app-0",
			state:  "running",
			ip:     "10.88.100.10",
			mac:    "02:00:00:00:00:10",
		}
	}

	pod := newTestPod("app-0", map[string]string{
		AnnMode:    "clone",
		AnnImage:   "base-image",
		AnnManaged: "true",
		AnnOS:      "linux",
		AnnVMID:    "vm-123",
		AnnVMName:  "vk-testns-app-0",
	})

	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod recovered existing VM: %v", err)
	}

	key := podKey(pod.Namespace, pod.Name)
	vm := p.vms[key]
	if vm == nil {
		t.Fatalf("expected recovered VM record for %s", key)
	}
	if vm.vmID != "vm-123" {
		t.Fatalf("recovered VMID = %q, want vm-123", vm.vmID)
	}
	if vm.vmName != "vk-testns-app-0" {
		t.Fatalf("recovered VMName = %q, want vk-testns-app-0", vm.vmName)
	}
	if got := p.pods[key].Annotations[AnnIP]; got != "10.88.100.10" {
		t.Fatalf("stored pod IP = %q, want 10.88.100.10", got)
	}
}

func TestCreatePodRecoversExistingManagedVMByNameFallback(t *testing.T) {
	p := newTestProvider()
	p.discoverVMByIDFn = func(_ context.Context, _ string) *CocoonVM { return nil }
	p.discoverVMFn = func(_ context.Context, vmName string) *CocoonVM {
		if vmName != "vk-testns-app-1" {
			t.Fatalf("discoverVM got %q, want vk-testns-app-1", vmName)
		}
		return &CocoonVM{
			vmID:   "vm-456",
			vmName: vmName,
			state:  "running",
			ip:     "10.88.100.11",
		}
	}

	pod := newTestPod("app-1", map[string]string{
		AnnMode:    "clone",
		AnnImage:   "base-image",
		AnnManaged: "true",
		AnnOS:      "linux",
		AnnVMID:    "vm-stale",
		AnnVMName:  "vk-testns-app-1",
	})

	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod recovered VM by name: %v", err)
	}

	key := podKey(pod.Namespace, pod.Name)
	if got := p.vms[key].vmID; got != "vm-456" {
		t.Fatalf("recovered VMID = %q, want vm-456", got)
	}
}

func TestCreatePodRecoversManagedVMWhenDiscoveryInitiallyReturnsStale(t *testing.T) {
	oldAttempts := managedRecoveryAttempts
	oldInterval := managedRecoveryInterval
	managedRecoveryAttempts = 2
	managedRecoveryInterval = 0
	defer func() {
		managedRecoveryAttempts = oldAttempts
		managedRecoveryInterval = oldInterval
	}()

	p := newTestProvider()
	var calls int
	p.discoverVMByIDFn = func(_ context.Context, vmID string) *CocoonVM {
		calls++
		if vmID != "vm-789" {
			t.Fatalf("discoverVMByID got %q, want vm-789", vmID)
		}
		if calls == 1 {
			return &CocoonVM{
				vmID:   vmID,
				vmName: "vk-testns-app-stale",
				state:  "stopped (stale)",
			}
		}
		return &CocoonVM{
			vmID:   vmID,
			vmName: "vk-testns-app-stale",
			state:  "running",
			ip:     "10.88.100.12",
		}
	}

	pod := newTestPod("app-stale", map[string]string{
		AnnMode:    "clone",
		AnnImage:   "base-image",
		AnnManaged: "true",
		AnnOS:      "linux",
		AnnVMID:    "vm-789",
		AnnVMName:  "vk-testns-app-stale",
	})

	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod recovered existing stale VM: %v", err)
	}
	if calls != 2 {
		t.Fatalf("discoverVMByID calls = %d, want 2", calls)
	}
}

func TestCreatePodRecoversHibernatedPodWithoutCreatingVM(t *testing.T) {
	p := newTestProvider()
	p.lookupSuspendedSnapshotFn = func(_ context.Context, ns, vmName string) string {
		if ns != "testns" || vmName != "vk-testns-app-2" {
			t.Fatalf("lookupSuspendedSnapshot got ns=%q vmName=%q", ns, vmName)
		}
		return "epoch.local/vk-testns-app-2-suspend"
	}

	pod := newTestPod("app-2", map[string]string{
		AnnMode:      "clone",
		AnnImage:     "base-image",
		AnnManaged:   "true",
		AnnOS:        "linux",
		AnnHibernate: "true",
		AnnVMID:      "vm-old",
		AnnVMName:    "vk-testns-app-2",
	})

	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod recovered hibernated pod: %v", err)
	}

	key := podKey(pod.Namespace, pod.Name)
	vm := p.vms[key]
	if vm == nil {
		t.Fatalf("expected hibernated VM record for %s", key)
	}
	if vm.state != "hibernated" {
		t.Fatalf("recovered state = %q, want hibernated", vm.state)
	}
	if vm.vmID != "" {
		t.Fatalf("hibernated VMID = %q, want empty", vm.vmID)
	}
}

func TestCreatePodDoesNotTreatFreshVMNameAsRecovery(t *testing.T) {
	p := newTestProvider()

	pod := newTestPod("app-3", map[string]string{
		AnnMode:   "clone",
		AnnImage:  "base-image",
		AnnVMName: "vk-testns-app-3",
	})

	if err := p.CreatePod(context.Background(), pod); err == nil {
		t.Fatalf("expected create path to run for fresh pod without %s", AnnVMID)
	}

	key := podKey(pod.Namespace, pod.Name)
	if _, ok := p.vms[key]; ok {
		t.Fatalf("unexpected VM record retained for failed fresh create")
	}
}
