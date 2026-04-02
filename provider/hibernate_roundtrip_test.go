package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestHibernateAndWakeVMRoundTrip(t *testing.T) {
	pod := newTestPod("app-1", map[string]string{
		AnnMode:  modeClone,
		AnnImage: "base-image",
		AnnOS:    osWindows,
	})

	p := newTestProviderWithClient(pod.DeepCopy())
	p.lookupSuspendedSnapshotFn = nil
	p.kubeClient = k8sfake.NewSimpleClientset(pod.DeepCopy())
	p.waitForDHCPIPFn = func(_ context.Context, vm *CocoonVM, _ time.Duration) string {
		return vm.ip
	}

	var cloneStarted bool
	p.cocoonExecFn = func(_ context.Context, args ...string) (string, error) {
		switch {
		case len(args) >= 2 && args[0] == "snapshot" && args[1] == "save":
			return "snapshot saved", nil
		case len(args) >= 2 && args[0] == "snapshot" && args[1] == "rm":
			return "", nil
		case len(args) >= 3 && args[0] == "vm" && args[1] == "rm":
			return "", nil
		case len(args) >= 3 && args[0] == "vm" && args[1] == "clone":
			cloneStarted = true
			return "clone ok", nil
		default:
			return "", nil
		}
	}

	p.discoverVMFn = func(_ context.Context, name string) *CocoonVM {
		if !cloneStarted {
			return nil
		}
		return &CocoonVM{
			vmID:   "vm-new",
			vmName: name,
			ip:     "10.88.100.42",
			mac:    "02:00:00:00:00:42",
			state:  stateRunning,
		}
	}

	key := podKey(pod.Namespace, pod.Name)
	vm := &CocoonVM{
		podNamespace: pod.Namespace,
		podName:      pod.Name,
		vmID:         "vm-old",
		vmName:       "vk-testns-app-1",
		ip:           "10.88.100.11",
		state:        stateRunning,
		os:           osWindows,
		managed:      true,
	}
	p.pods[key] = pod.DeepCopy()
	p.vms[key] = vm

	p.hibernateVM(context.Background(), pod, vm)
	if vm.state != stateHibernated {
		t.Fatalf("hibernate state = %q, want %q", vm.state, stateHibernated)
	}
	if vm.vmID != "" {
		t.Fatalf("hibernate vmID = %q, want empty", vm.vmID)
	}

	ref := p.snapshotManager().lookupSuspendedSnapshot(context.Background(), pod.Namespace, vm.vmName)
	if !strings.Contains(ref, vm.vmName+"-suspend") {
		t.Fatalf("suspended snapshot ref = %q, want %q suffix", ref, vm.vmName+"-suspend")
	}

	p.wakeVM(context.Background(), pod, vm)
	if vm.state != stateRunning {
		t.Fatalf("wake state = %q, want %q", vm.state, stateRunning)
	}
	if vm.vmID != "vm-new" {
		t.Fatalf("wake vmID = %q, want vm-new", vm.vmID)
	}
	if vm.ip != "10.88.100.42" {
		t.Fatalf("wake ip = %q, want 10.88.100.42", vm.ip)
	}
	if got := p.snapshotManager().lookupSuspendedSnapshot(context.Background(), pod.Namespace, vm.vmName); got != "" {
		t.Fatalf("suspended snapshot should be cleared after wake, got %q", got)
	}
}
