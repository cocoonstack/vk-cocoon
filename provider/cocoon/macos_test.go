package cocoon

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"slices"
	"strconv"
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
	"tap": "bt12345678-0",
	"disk": "/var/lib/cocoon-macos/vms/macos-demo/disk.qcow2",
	"vnc": 7
}`

func TestIsMacosSpec(t *testing.T) {
	if !isMacosSpec(meta.VMSpec{OS: "macos"}) || !isMacosSpec(meta.VMSpec{OS: "MacOS"}) {
		t.Fatal("os=macos spec not detected")
	}
	if isMacosSpec(meta.VMSpec{OS: "windows"}) || isMacosSpec(meta.VMSpec{}) {
		t.Fatal("non-macos spec misdetected")
	}
}

func TestClaimMacosVNCPort(t *testing.T) {
	p := newTestProvider(t)
	if got := p.claimMacosVNCPort("ns/a", 0); got != 5900 {
		t.Errorf("first claim = %d, want 5900", got)
	}
	if got := p.claimMacosVNCPort("ns/b", 0); got != 5901 {
		t.Errorf("second claim = %d, want 5901", got)
	}
	if got := p.claimMacosVNCPort("ns/a", 0); got != 5900 {
		t.Errorf("replayed claim = %d, want idempotent 5900", got)
	}
	if got := p.claimMacosVNCPort("ns/c", 5950); got != 5950 {
		t.Errorf("preferred free port = %d, want 5950", got)
	}
	if got := p.claimMacosVNCPort("ns/d", 5950); got != 5902 {
		t.Errorf("preferred taken port = %d, want next free 5902", got)
	}
	p.forgetPod("ns", "a")
	if got := p.claimMacosVNCPort("ns/e", 0); got != 5900 {
		t.Errorf("claim after release = %d, want freed 5900", got)
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
		"--storage 107374182400",
		"--exit-on-reboot",
		"--vnc 0",
		"--vnc-password testpass",
		"--random-smbios",
		"--net cni",
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
	if got.Annotations[meta.AnnotationVNCPort] != "5900" {
		t.Errorf("VNC port annotation = %q, want 5900", got.Annotations[meta.AnnotationVNCPort])
	}
	if state := meta.ReadLifecycleState(got); state != meta.LifecycleStateCreating {
		t.Errorf("lifecycle state = %q, want creating", state)
	}
}

func TestMacosProbeUsesDeclaredProbePort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)

	p := newTestProvider(t)
	spec := macosSpec()
	spec.ProbePort = port
	pod := newPodWithSpec(spec)
	p.trackPod(pod, &vm.VM{
		ID: macosVMID("macos-demo"), Name: "macos-demo", Hypervisor: macosHypervisor,
		State: vm.StateRunning, IP: "127.0.0.1", MAC: "52:54:00:12:34:56",
	})

	probe := p.buildMacosProbe("ns", "demo-0")
	if ready, msg := probe(t.Context()); !ready || msg != "tcp ok" {
		t.Fatalf("declared probe port should make macOS ready, got ready=%v msg=%q", ready, msg)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	if ready, msg := probe(t.Context()); ready || !strings.Contains(msg, "tcp probe "+port) {
		t.Fatalf("closed declared probe port should make macOS unready, got ready=%v msg=%q", ready, msg)
	}
}

func TestConfigureMacosLifecycleCommandUsesGracefulCancellation(t *testing.T) {
	cmd := exec.Command("true")
	configureMacosLifecycleCommand(cmd)
	if cmd.Cancel == nil {
		t.Fatal("launch command has no graceful cancel function")
	}
	if cmd.WaitDelay != macosCommandCleanupGrace {
		t.Fatalf("launch cleanup grace = %v, want %v", cmd.WaitDelay, macosCommandCleanupGrace)
	}
}

func TestFormatMacosArgsForLogRedactsPassword(t *testing.T) {
	got := formatMacosArgsForLog([]string{"vm", "start", "--vnc", "1", "--vnc-password", "secret", "demo"})
	if strings.Contains(got, "secret") || !strings.Contains(got, "--vnc-password <redacted>") {
		t.Fatalf("redacted args = %q", got)
	}
}

func TestAppendMacosVNCArgsRequiresPassword(t *testing.T) {
	p := newTestProvider(t)
	p.MacosVNCPassword = ""
	if _, err := p.appendMacosVNCArgs([]string{"vm", "start"}, 5900); err == nil {
		t.Fatal("expected missing VNC password to be rejected")
	}
}

func TestAppendMacosVNCArgsRejectsLongPassword(t *testing.T) {
	p := newTestProvider(t)
	p.MacosVNCPassword = "123456789"
	if _, err := p.appendMacosVNCArgs([]string{"vm", "start"}, 5900); err == nil {
		t.Fatal("expected VNC password longer than 8 bytes to be rejected")
	}
}

func TestDeleteMacosPodStopsProbeBeforeRemove(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	p.Clientset = fake.NewSimpleClientset(pod)
	stubMacosExec(p, freshCreateHandler)
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	key := meta.PodKey(pod.Namespace, pod.Name)
	if p.Probes.Get(key).LastSeen.IsZero() {
		t.Fatal("probe agent was not armed by CreatePod")
	}

	p.macosExecFn = func(_ context.Context, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "vm" && args[1] == "rm" {
			return "", errors.New("rm interrupted")
		}
		return freshCreateHandler(args)
	}
	if err := p.DeletePod(t.Context(), pod); err == nil {
		t.Fatal("DeletePod must surface the rm failure")
	}
	if !p.Probes.Get(key).LastSeen.IsZero() {
		t.Fatal("probe agent survived a failed rm; its crash recovery would vm start the guest mid-teardown")
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
	if v.DiskPath != "/var/lib/cocoon-macos/vms/macos-demo/disk.qcow2" {
		t.Errorf("adopted VM missing disk path: %q", v.DiskPath)
	}
	if len(v.NetworkConfigs) != 1 || v.NetworkConfigs[0].Tap != "bt12345678-0" {
		t.Fatalf("adopted VM missing tap network config: %+v", v.NetworkConfigs)
	}
	got, err := p.Clientset.CoreV1().Pods("ns").Get(t.Context(), "demo-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if got.Annotations[meta.AnnotationVNCPort] != "5907" {
		t.Errorf("VNC port annotation = %q, want adopted 5907", got.Annotations[meta.AnnotationVNCPort])
	}
}

func TestCreateMacosPodStartsDeadRecord(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	p.Clientset = fake.NewSimpleClientset(pod)
	started := false
	p.macosProcessAliveFn = func(int) bool { return started }
	calls := stubMacosExec(p, func(args []string) (string, error) {
		if macosCallIs(args, "vm", "start") {
			started = true
			return "macos-demo (pid 4242)\n", nil
		}
		return inspectOnlyHandler(args)
	})

	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	all := calls()
	starts := macosCallsWithPrefix(all, "vm", "start")
	if len(starts) != 1 {
		t.Fatalf("dead record must dispatch `vm start`, got %v", all)
	}
	if joined := strings.Join(starts[0], " "); !strings.Contains(joined, "--vnc 0 --vnc-password testpass") {
		t.Errorf("`vm start` must re-assert protected VNC, got: %s", joined)
	} else if strings.Contains(joined, "--exit-on-reboot") || strings.Contains(joined, "--storage") {
		t.Errorf("`vm start` must rely on persisted storage/reboot policy, got: %s", joined)
	}
	if len(macosCallsWithPrefix(all, "vm", "run")) != 0 {
		t.Fatalf("dead record must not relaunch via `vm run` (disk corruption), got %v", all)
	}
}

func TestCreateMacosPodsAllocateDistinctVNCPorts(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	spec2 := macosSpec()
	spec2.VMName = "macos-demo-2"
	pod2 := newPodWithSpec(spec2)
	pod2.Name = "demo-1"
	p.Clientset = fake.NewSimpleClientset(pod, pod2)
	calls := stubMacosExec(p, freshCreateHandler)

	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("first CreatePod: %v", err)
	}
	if err := p.CreatePod(t.Context(), pod2); err != nil {
		t.Fatalf("second CreatePod: %v", err)
	}
	runs := macosCallsWithPrefix(calls(), "vm", "run")
	if len(runs) != 2 {
		t.Fatalf("want 2 vm runs, got %v", runs)
	}
	first, second := strings.Join(runs[0], " "), strings.Join(runs[1], " ")
	if !strings.Contains(first, "--vnc 0") || !strings.Contains(second, "--vnc 1") {
		t.Fatalf("same-node guests must get distinct displays:\n %s\n %s", first, second)
	}
}

func TestMacosProbeRestartsDeadQemu(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	p.Clientset = fake.NewSimpleClientset(pod)
	p.macosProcessAliveFn = func(pid int) bool { return pid == 4243 }
	started := false
	calls := stubMacosExec(p, func(args []string) (string, error) {
		switch {
		case macosCallIs(args, "vm", "start"):
			started = true
			return "macos-demo (pid 4243)\n", nil
		case macosCallIs(args, "vm", "inspect"):
			if started {
				return strings.Replace(macosInspectJSON, "4242", "4243", 1), nil
			}
			return macosInspectJSON, nil
		default:
			return "", nil
		}
	})
	p.trackPod(pod, &vm.VM{
		ID: macosVMID("macos-demo"), Name: "macos-demo", Hypervisor: macosHypervisor,
		State: vm.StateRunning, PID: 4242, MAC: "52:54:00:12:34:56",
	})

	probe := p.buildMacosProbe("ns", "demo-0")
	if ready, msg := probe(t.Context()); ready || !strings.Contains(msg, "restarted") {
		t.Fatalf("dead qemu must trigger a restart, got ready=%v msg=%q", ready, msg)
	}
	if starts := macosCallsWithPrefix(calls(), "vm", "start"); len(starts) != 1 {
		t.Fatalf("want one vm start, got %v", starts)
	}
	if v := p.vmForPod("ns", "demo-0"); v == nil || v.PID != 4243 {
		t.Fatalf("record not refreshed after restart: %+v", v)
	}
	if ready, msg := probe(t.Context()); ready || !strings.Contains(msg, "lease") {
		t.Fatalf("restarted guest should wait for its lease, got ready=%v msg=%q", ready, msg)
	}
	if starts := macosCallsWithPrefix(calls(), "vm", "start"); len(starts) != 1 {
		t.Fatalf("live guest must not be restarted again, got %v", starts)
	}
}

func TestMacosRecoverAdoptsSlowStartedQemu(t *testing.T) {
	p := newTestProvider(t)
	pod := newPodWithSpec(macosSpec())
	p.Clientset = fake.NewSimpleClientset(pod)
	started := false
	inspects := 0
	p.macosProcessAliveFn = func(pid int) bool { return started && pid == 4243 }
	calls := stubMacosExec(p, func(args []string) (string, error) {
		switch {
		case macosCallIs(args, "vm", "start"):
			started = true
			return "macos-demo (pid 4243)\n", nil
		case macosCallIs(args, "vm", "inspect"):
			inspects++
			if inspects == 2 {
				return "", errors.New("context deadline exceeded")
			}
			if started {
				return strings.Replace(macosInspectJSON, "4242", "4243", 1), nil
			}
			return macosInspectJSON, nil
		default:
			return "", nil
		}
	})
	p.trackPod(pod, &vm.VM{
		ID: macosVMID("macos-demo"), Name: "macos-demo", Hypervisor: macosHypervisor,
		State: vm.StateRunning, PID: 4242, MAC: "52:54:00:12:34:56",
	})

	v := p.vmForPod("ns", "demo-0")
	if msg := p.recoverMacosVM(t.Context(), "ns", "demo-0", v); !strings.Contains(msg, "restarted") {
		t.Fatalf("first round must start the dead record, got %q", msg)
	}
	if v = p.vmForPod("ns", "demo-0"); v == nil || v.PID != 4242 {
		t.Fatalf("failed post-start inspect must leave the record unchanged, got %+v", v)
	}
	if msg := p.recoverMacosVM(t.Context(), "ns", "demo-0", v); !strings.Contains(msg, "adopted") {
		t.Fatalf("second round must adopt the running qemu, got %q", msg)
	}
	if v = p.vmForPod("ns", "demo-0"); v == nil || v.PID != 4243 {
		t.Fatalf("adoption must refresh the PID, got %+v", v)
	}
	if starts := macosCallsWithPrefix(calls(), "vm", "start"); len(starts) != 1 {
		t.Fatalf("second round must adopt, never start against the live qemu's VNC port: %v", starts)
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
	releaser := &recordingLeaseReleaser{}
	p.LeaseReleaser = releaser
	p.trackPod(pod, &vm.VM{ID: macosVMID("macos-demo"), Name: "macos-demo", Hypervisor: macosHypervisor, State: vm.StateRunning, MAC: "52:54:00:12:34:56"})
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
	if !slices.Equal(releaser.macs, []string{"52:54:00:12:34:56"}) {
		t.Errorf("released MACs = %v", releaser.macs)
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
		Storage:   "100Gi",
		ProbePort: "2275",
	}
}

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
