package cocoon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"

	"github.com/cocoonstack/vk-cocoon/vm"
)

const macosInspectJSON = `{
	"name": "macos-demo",
	"image": "macos-tahoe-26-img",
	"pid": 4242,
	"mac": "52:54:00:12:34:56",
	"tap": "bt12345678-0"
}`

func TestIsMacosSpec(t *testing.T) {
	if !isMacosSpec(meta.VMSpec{OS: "macos"}) || !isMacosSpec(meta.VMSpec{OS: "MacOS"}) {
		t.Fatal("os=macos spec not detected")
	}
	if isMacosSpec(meta.VMSpec{OS: "windows"}) || isMacosSpec(meta.VMSpec{}) {
		t.Fatal("non-macos spec misdetected")
	}
}

func TestMacosVNCDisplay(t *testing.T) {
	for _, tc := range []struct {
		slot string
		want int
	}{
		{"2275", 75},
		{"", 22},      // default slot 2222
		{"2200", 0},   // x00 slot maps to display 0 (port 5900); mod-100 stays injective
		{"abc", 22},   // unparseable falls back to the default slot
		{"70000", 22}, // out of range
	} {
		if got := macosVNCDisplay(meta.VMSpec{ProbePort: tc.slot}); got != tc.want {
			t.Errorf("macosVNCDisplay(%q) = %d, want %d", tc.slot, got, tc.want)
		}
	}
}

func TestCreateMacosPodDispatchesRunAndRegisters(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	p.Clientset = fake.NewSimpleClientset(pod)
	calls := stubMacosExec(p, freshCreateHandler)

	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	runs := macosCallsWithPrefix(calls(), "vm", "run")
	if len(runs) != 1 {
		t.Fatalf("vm run dispatched %d times, want 1; calls=%v", len(runs), calls())
	}
	joined := strings.Join(runs[0], " ")
	for _, want := range []string{
		"--name macos-demo",
		"--cpus 4",
		"--memory 8192",
		"--vnc 75",
		"--random-smbios",
		"--net tap --bridge cni0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("run argv missing %q\n got: %s", want, joined)
		}
	}
	if strings.Contains(joined, "--ssh-port") {
		t.Errorf("SLIRP host-forward must never be scheduled\n got: %s", joined)
	}
	if last := runs[0][len(runs[0])-1]; last != "macos-tahoe-26-img" {
		t.Errorf("image must be the final positional arg, got %q", last)
	}

	v := p.vmForPod("ns", "demo-0")
	if v == nil {
		t.Fatal("VM not tracked after CreatePod")
	}
	if v.ID != "qemu-macos-demo" || v.Hypervisor != macosHypervisor || v.State != vm.StateRunning {
		t.Errorf("unexpected VM record: id=%s hypervisor=%s state=%s", v.ID, v.Hypervisor, v.State)
	}

	status, err := p.GetPodStatus(t.Context(), "ns", "demo-0")
	if err != nil {
		t.Fatalf("GetPodStatus: %v", err)
	}
	if status.Phase != "Running" {
		t.Errorf("pod phase = %s, want Running", status.Phase)
	}

	got, err := p.Clientset.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got.Annotations[meta.AnnotationVMID] != "qemu-macos-demo" {
		t.Errorf("VMID annotation = %q", got.Annotations[meta.AnnotationVMID])
	}
	if got.Annotations[meta.AnnotationVNCPort] != "5975" {
		t.Errorf("VNC port annotation = %q, want 5975", got.Annotations[meta.AnnotationVNCPort])
	}
	// Ready is deferred to the SSH probe, so lifecycle must still be creating.
	if state := meta.ReadLifecycleState(got); state != meta.LifecycleStateCreating {
		t.Errorf("lifecycle state = %q, want creating", state)
	}
}

func TestCreateMacosPodSkipsDuplicateRun(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	p.Clientset = fake.NewSimpleClientset(pod)
	calls := stubMacosExec(p, freshCreateHandler)

	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("first CreatePod: %v", err)
	}
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("duplicate CreatePod: %v", err)
	}
	if runs := macosCallsWithPrefix(calls(), "vm", "run"); len(runs) != 1 {
		t.Fatalf("vm run dispatched %d times, want 1", len(runs))
	}
}

func TestCreateMacosPodAdoptsLiveVM(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	p.Clientset = fake.NewSimpleClientset(pod)
	p.macosProcessAliveFn = func(pid int) bool { return pid == 4242 }
	calls := stubMacosExec(p, inspectOnlyHandler)

	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	all := calls()
	if len(macosCallsWithPrefix(all, "vm", "run")) != 0 || len(macosCallsWithPrefix(all, "vm", "start")) != 0 {
		t.Fatalf("live adoption must not dispatch a lifecycle command, got %v", all)
	}
	v := p.vmForPod("ns", "demo-0")
	if v == nil || v.PID != 4242 || v.MAC != "52:54:00:12:34:56" {
		t.Fatalf("adopted VM record incomplete: %+v", v)
	}
	if len(v.NetworkConfigs) != 1 || v.NetworkConfigs[0].Tap != "bt12345678-0" {
		t.Fatalf("adopted VM missing tap network config: %+v", v.NetworkConfigs)
	}
}

func TestCreateMacosPodStartsDeadRecord(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	p.Clientset = fake.NewSimpleClientset(pod)
	p.macosProcessAliveFn = func(int) bool { return false }
	calls := stubMacosExec(p, inspectOnlyHandler)

	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	all := calls()
	starts := macosCallsWithPrefix(all, "vm", "start")
	if len(starts) != 1 {
		t.Fatalf("dead record must dispatch `vm start`, got %v", all)
	}
	// VNC is launch-scoped in cocoon-macos: a bare `vm start` disables it while
	// the vnc-port annotation still advertises the display.
	if joined := strings.Join(starts[0], " "); !strings.Contains(joined, "--vnc 75") {
		t.Errorf("`vm start` must re-assert the VNC display, got: %s", joined)
	}
	if len(macosCallsWithPrefix(all, "vm", "run")) != 0 {
		t.Fatalf("dead record must not relaunch via `vm run` (disk corruption), got %v", all)
	}
}

func TestCreateMacosPodRunFailureKeepsSameNameVM(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	p.Clientset = fake.NewSimpleClientset(pod)
	calls := stubMacosExec(p, func(args []string) (string, error) {
		switch {
		case macosCallIs(args, "vm", "inspect"):
			return "", errors.New("transient inspect failure")
		case macosCallIs(args, "image", "inspect"):
			return "{}", nil
		case macosCallIs(args, "vm", "run"):
			return "macOS VM macos-demo already exists", errors.New("exit status 1")
		default:
			return "", nil
		}
	})

	err := p.CreatePod(t.Context(), pod)
	if err == nil || !strings.Contains(err.Error(), "vm run") {
		t.Fatalf("expected vm run failure, got %v", err)
	}
	// The inspect failure is indistinguishable from a missing record, so the
	// collision is not proof this call created the VM: never clean up with rm.
	if rms := macosCallsWithPrefix(calls(), "vm", "rm"); len(rms) != 0 {
		t.Fatalf("ambiguous failed run removed a same-name VM: %v", rms)
	}
	if p.vmForPod("ns", "demo-0") != nil {
		t.Fatal("failed launch must not leave a VM tracked")
	}
	if _, err := p.GetPod(t.Context(), "ns", "demo-0"); err == nil {
		t.Fatal("failed launch must drop the pod claim so the create retry starts clean")
	}
}

func TestCreateMacosPodAutoPullsWhenImageMissing(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	p.Clientset = fake.NewSimpleClientset(pod)
	pulled := false
	calls := stubMacosExec(p, func(args []string) (string, error) {
		switch {
		case macosCallIs(args, "vm", "inspect"):
			return "", errors.New("no record")
		case macosCallIs(args, "image", "inspect"):
			if pulled {
				return "{}", nil
			}
			return "", errors.New("image not found")
		case macosCallIs(args, "image", "pull"):
			pulled = true
			return "pulled macos-tahoe-26-img", nil
		case macosCallIs(args, "vm", "run"):
			return "macos-demo (pid 4242)\n", nil
		default:
			return "", nil
		}
	})

	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("CreatePod should auto-pull then launch, got %v", err)
	}
	all := calls()
	if len(macosCallsWithPrefix(all, "image", "pull")) != 1 {
		t.Errorf("expected one `image pull` for a missing image, calls=%v", all)
	}
	if len(macosCallsWithPrefix(all, "vm", "run")) != 1 {
		t.Errorf("expected `vm run` after the auto-pull, calls=%v", all)
	}
}

func TestCreateMacosPodFailsWhenAutoPullFails(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	p.Clientset = fake.NewSimpleClientset(pod)
	stubMacosExec(p, func(args []string) (string, error) {
		if macosCallIs(args, "image", "pull") {
			return "registry unreachable", errors.New("exit status 1")
		}
		return "", errors.New("not found")
	})

	err := p.CreatePod(t.Context(), pod)
	if err == nil || !strings.Contains(err.Error(), "not materialized") {
		t.Fatalf("expected auto-pull failure, got %v", err)
	}
	if p.vmForPod("ns", "demo-0") != nil {
		t.Fatal("VM must not be tracked when the pull fails")
	}
}

func TestDeleteMacosPodRemovesVM(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	p.Clientset = fake.NewSimpleClientset(pod)
	p.trackPod(pod, &vm.VM{ID: macosVMID("macos-demo"), Name: "macos-demo", Hypervisor: macosHypervisor, State: vm.StateRunning})
	calls := stubMacosExec(p, func(args []string) (string, error) { return "", nil })

	if err := p.DeletePod(t.Context(), pod); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	rms := macosCallsWithPrefix(calls(), "vm", "rm")
	if len(rms) != 1 || rms[0][2] != "macos-demo" {
		t.Fatalf("expected one `vm rm macos-demo`, got %v", calls())
	}
	if p.vmForPod("ns", "demo-0") != nil {
		t.Fatal("VM still tracked after delete")
	}
}

func TestDeleteMacosPodToleratesMissingVM(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	p.Clientset = fake.NewSimpleClientset(pod)
	p.trackPod(pod, &vm.VM{ID: macosVMID("macos-demo"), Name: "macos-demo", Hypervisor: macosHypervisor, State: vm.StateRunning})
	stubMacosExec(p, func(args []string) (string, error) {
		return "Error: VM macos-demo not found", errors.New("exit status 1")
	})

	if err := p.DeletePod(t.Context(), pod); err != nil {
		t.Fatalf("delete of an already-removed VM must succeed, got %v", err)
	}
	if p.vmForPod("ns", "demo-0") != nil {
		t.Fatal("VM still tracked after delete")
	}
}

func TestUpdateMacosPodRejectsHibernate(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	meta.HibernateState(true).Apply(pod)
	p.Clientset = fake.NewSimpleClientset(pod)
	stubMacosExec(p, func(args []string) (string, error) { return "", nil })
	p.trackPod(pod, &vm.VM{ID: macosVMID("macos-demo"), Name: "macos-demo", Hypervisor: macosHypervisor, State: vm.StateRunning})

	err := p.UpdatePod(t.Context(), pod)
	if err == nil || !strings.Contains(err.Error(), "does not support hibernate") {
		t.Fatalf("expected hibernate rejection, got %v", err)
	}
	got, getErr := p.Clientset.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("get pod: %v", getErr)
	}
	if state := meta.ReadLifecycleState(got); state != meta.LifecycleStateFailed {
		t.Errorf("lifecycle state = %q, want failed", state)
	}
}

func TestStartupReconcileAdoptsLiveMacosVM(t *testing.T) {
	p := newTestProvider(t)
	p.NodeName = "n1"
	pod := newPodWithSpec(macosSpec())
	pod.Spec.NodeName = "n1"
	p.Clientset = fake.NewSimpleClientset(pod)
	p.Runtime = &fakeRuntime{}
	p.macosProcessAliveFn = func(pid int) bool { return pid == 4242 }
	calls := stubMacosExec(p, inspectOnlyHandler)

	if err := p.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}
	v := p.vmForPod("ns", "demo-0")
	if v == nil || v.PID != 4242 || v.Hypervisor != macosHypervisor {
		t.Fatalf("live macOS VM not adopted at startup: %+v", v)
	}
	if all := calls(); len(macosCallsWithPrefix(all, "vm", "run")) != 0 || len(macosCallsWithPrefix(all, "vm", "start")) != 0 {
		t.Fatalf("startup adoption must not dispatch a lifecycle command, got %v", all)
	}
	if p.Probes.Get(meta.PodKey("ns", "demo-0")).LastSeen.IsZero() {
		t.Fatal("readiness probe not started for the adopted macOS pod")
	}
}

func macosSpec() meta.VMSpec {
	return meta.VMSpec{
		VMName:    "macos-demo",
		Image:     "macos-tahoe-26-img",
		OS:        string(cocoonv1.OSMacos),
		ProbePort: "2275",
	}
}

// stubMacosExec records every cocoon-macos dispatch and answers via handler.
func stubMacosExec(p *Provider, handler func(args []string) (string, error)) func() [][]string {
	var mu sync.Mutex
	var calls [][]string
	p.macosExecFn = func(_ context.Context, args ...string) (string, error) {
		mu.Lock()
		calls = append(calls, args)
		mu.Unlock()
		return handler(args)
	}
	return func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

// freshCreateHandler answers no record, image present, launch OK — the
// fresh `vm run` path.
func freshCreateHandler(args []string) (string, error) {
	switch {
	case macosCallIs(args, "vm", "inspect"):
		return "", errors.New("no record")
	case macosCallIs(args, "image", "inspect"):
		return "{}", nil
	case macosCallIs(args, "vm", "run"):
		return "macos-demo (pid 4242)\n", nil
	default:
		return "", nil
	}
}

func inspectOnlyHandler(args []string) (string, error) {
	if macosCallIs(args, "vm", "inspect") {
		return macosInspectJSON, nil
	}
	return "", nil
}

func macosCallIs(args []string, verb, sub string) bool {
	return len(args) >= 2 && args[0] == verb && args[1] == sub
}

func macosCallsWithPrefix(calls [][]string, verb, sub string) [][]string {
	var out [][]string
	for _, c := range calls {
		if macosCallIs(c, verb, sub) {
			out = append(out, c)
		}
	}
	return out
}
