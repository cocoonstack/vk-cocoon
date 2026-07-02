package cocoon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/guest"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestNeedsPostClone(t *testing.T) {
	t.Parallel()

	staticNIC := []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff", Network: &vm.NetworkInfo{IP: "10.0.0.2", Prefix: 24, Gateway: "10.0.0.1"}}}
	dhcpNIC := []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff"}}

	cases := []struct {
		name    string
		backend string
		nics    []*vm.NetworkConfig
		want    bool
	}{
		{name: "CH + DHCP (auto)", backend: "cloud-hypervisor", nics: dhcpNIC, want: false},
		{name: "CH + Static", backend: "cloud-hypervisor", nics: staticNIC, want: true},
		{name: "FC + DHCP", backend: "firecracker", nics: dhcpNIC, want: true},
		{name: "FC + Static", backend: "firecracker", nics: staticNIC, want: true},
		{name: "CH + no NICs", backend: "cloud-hypervisor", nics: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := needsPostClone(tc.backend, tc.nics)
			if got != tc.want {
				t.Errorf("needsPostClone(%q, %d nics) = %v, want %v", tc.backend, len(tc.nics), got, tc.want)
			}
		})
	}
}

func TestBuildPostCloneCommands(t *testing.T) {
	t.Parallel()

	t.Run("FC + static + 2 NICs", func(t *testing.T) {
		nics := []*vm.NetworkConfig{
			{MAC: "aa:bb:cc:00:11:22", Network: &vm.NetworkInfo{IP: "10.0.0.2", Prefix: 24, Gateway: "10.0.0.1"}},
			{MAC: "aa:bb:cc:00:11:33", Network: &vm.NetworkInfo{IP: "10.0.0.3", Prefix: 24, Gateway: "10.0.0.1"}},
		}
		cmds := buildPostCloneCommands("my-vm", vm.BackendFirecracker, "x", "", nics)

		if !strings.Contains(cmds, "ip link set dev eth0") {
			t.Errorf("missing MAC fixup for eth0")
		}
		if !strings.Contains(cmds, "ip link set dev eth1") {
			t.Errorf("missing MAC fixup for eth1")
		}
		if !strings.Contains(cmds, "Address=10.0.0.2/24") {
			t.Errorf("missing static config for NIC 0")
		}
	})

	t.Run("CH + DHCP + 1 NIC", func(t *testing.T) {
		nics := []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff"}}
		cmds := buildPostCloneCommands("clone-vm", "cloud-hypervisor", "x", "", nics)

		if strings.Contains(cmds, "ip link set") {
			t.Errorf("CH should not have MAC fixup")
		}
		if !strings.Contains(cmds, "DHCP=ipv4") {
			t.Errorf("missing DHCP config")
		}
	})

	t.Run("CH + cloudimg (from sourceImage)", func(t *testing.T) {
		nics := []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff"}}
		cmds := buildPostCloneCommands("ci-vm", "cloud-hypervisor", "x", "https://cloud-images.ubuntu.com/img.img", nics)

		if !strings.Contains(cmds, "cloud-init clean") {
			t.Errorf("cloudimg should use cloud-init clean")
		}
		if !strings.Contains(cmds, "cloud-init init") {
			t.Errorf("cloudimg should use cloud-init init")
		}
		if strings.Contains(cmds, "printf") {
			t.Errorf("cloudimg should not write networkd files directly")
		}
	})

	t.Run("FC + mixed static/DHCP", func(t *testing.T) {
		nics := []*vm.NetworkConfig{
			{MAC: "aa:00:00:00:00:01", Network: &vm.NetworkInfo{IP: "10.0.0.5", Prefix: 16, Gateway: "10.0.0.1"}},
			{MAC: "aa:00:00:00:00:02"},
		}
		cmds := buildPostCloneCommands("mixed", vm.BackendFirecracker, "x", "", nics)

		if !strings.Contains(cmds, "Address=10.0.0.5/16") {
			t.Errorf("missing static config")
		}
		if !strings.Contains(cmds, "DHCP=ipv4") {
			t.Errorf("missing DHCP config for second NIC")
		}
	})
}

func TestPostCloneKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec meta.VMSpec
		want string
	}{
		{"windows is windows", meta.VMSpec{OS: "windows"}, postCloneKindWindows},
		{"linux+fc is linux_fc", meta.VMSpec{Backend: vm.BackendFirecracker}, postCloneKindLinuxFC},
		{"linux+ch is linux_static", meta.VMSpec{Backend: "cloud-hypervisor"}, postCloneKindLinuxStatic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := postCloneKind(tc.spec); got != tc.want {
				t.Errorf("postCloneKind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("short string mutated: %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc" {
		t.Errorf("truncate failed: %q", got)
	}
	if got := truncate("", 10); got != "" {
		t.Errorf("empty mutated: %q", got)
	}
}

func TestPlanPostClone(t *testing.T) {
	t.Parallel()

	staticNIC := []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff", Network: &vm.NetworkInfo{IP: "10.0.0.2", Prefix: 24, Gateway: "10.0.0.1"}}}
	dhcpNIC := []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff"}}

	t.Run("Linux CH OCI DHCP — no plan", func(t *testing.T) {
		v := &vm.VM{ID: "x", NetworkConfigs: dhcpNIC}
		_, ok := planPostClone(meta.VMSpec{Backend: "cloud-hypervisor"}, v, "")
		if ok {
			t.Errorf("CH+OCI+DHCP should not need post-clone setup")
		}
	})

	t.Run("Linux CH cloudimg DHCP — no plan", func(t *testing.T) {
		v := &vm.VM{ID: "x", NetworkConfigs: dhcpNIC}
		_, ok := planPostClone(meta.VMSpec{Backend: "cloud-hypervisor"}, v, "https://cloud-images.ubuntu.com/img.img")
		if ok {
			t.Errorf("cloudimg+DHCP self-heals via netplan fallback; no plan expected")
		}
	})

	t.Run("Linux CH static — sh -c plan", func(t *testing.T) {
		v := &vm.VM{ID: "x", NetworkConfigs: staticNIC}
		plan, ok := planPostClone(meta.VMSpec{Backend: "cloud-hypervisor", VMName: "vm"}, v, "")
		if !ok {
			t.Fatalf("static-IP should produce a plan")
		}
		if len(plan.argv) != 3 || plan.argv[0] != "sh" || plan.argv[1] != "-c" {
			t.Errorf("Linux plan argv = %v, want [sh -c <script>]", plan.argv)
		}
		if !strings.Contains(plan.argv[2], "Address=10.0.0.2/24") {
			t.Errorf("script missing static IP config: %q", plan.argv[2])
		}
	})

	t.Run("Windows — PowerShell PnP-rebind", func(t *testing.T) {
		v := &vm.VM{ID: "x", NetworkConfigs: dhcpNIC}
		plan, ok := planPostClone(meta.VMSpec{OS: "windows"}, v, "")
		if !ok {
			t.Fatalf("Windows should always produce a plan (chained-clone NDIS unstick)")
		}
		if plan.argv[0] != "powershell" {
			t.Errorf("Windows plan argv[0] = %q, want powershell", plan.argv[0])
		}
		if !strings.Contains(plan.argv[3], "Get-PnpDevice -Class Net -PresentOnly") {
			t.Errorf("Windows script missing PnP-rebind: %q", plan.argv[3])
		}
		if !strings.Contains(plan.argv[3], "Disable-PnpDevice") || !strings.Contains(plan.argv[3], "Enable-PnpDevice") {
			t.Errorf("Windows script missing Disable/Enable cycle: %q", plan.argv[3])
		}
		if !strings.HasPrefix(plan.hint, "powershell -nop -c '") || !strings.HasSuffix(plan.hint, "'") {
			t.Errorf("Windows hint should single-quote the script body, got %q", plan.hint)
		}
	})
}

func TestCreatePodWindowsRunModeSACFailureKeepsFailed(t *testing.T) {
	rt := &fakeRuntime{
		runVM: &vm.VM{
			ID: "vmid", Name: "vk-ns-win-0",
			NetworkConfigs: []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff", Network: &vm.NetworkInfo{IP: "10.0.0.5", Prefix: 24, Gateway: "10.0.0.1"}}},
		},
	}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())
	p.GuestSAC = failingSACDialer{}

	pod := newPodWithSpec(meta.VMSpec{
		VMName: "vk-ns-win-0",
		OS:     "windows",
		Mode:   "run",
	})
	if err := p.CreatePod(t.Context(), pod); err != nil {
		t.Fatalf("create: %v", err)
	}
	p.Close() // drain bgWG so the SAC goroutine completes

	if got := pod.Annotations[meta.AnnotationLifecycleState]; got != string(meta.LifecycleStateFailed) {
		t.Errorf("lifecycle-state = %q, want %q (SAC Failed must not be clobbered by !cloned Ready)",
			got, meta.LifecycleStateFailed)
	}
	if got := pod.Annotations[annotationPostCloneState]; got != postCloneStateFailed {
		t.Errorf("post-clone-state = %q, want %q", got, postCloneStateFailed)
	}
}

func TestPostCloneErrorsAnnotationTruncated(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns", Annotations: map[string]string{}}}
	v := &vm.VM{ID: "vmid", NetworkConfigs: []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff", Network: &vm.NetworkInfo{IP: "10.0.0.5", Prefix: 24, Gateway: "10.0.0.1"}}}}
	p := newTestProvider(t)

	// Build enough errors that errors.Join's body comfortably exceeds 4 KiB.
	errs := make([]error, 0, 80)
	for i := range 80 {
		errs = append(errs, fmt.Errorf("attempt %d: %s", i, strings.Repeat("x", 100)))
	}
	p.emitPostCloneHint(t.Context(), pod, meta.VMSpec{Backend: "cloud-hypervisor", VMName: "vm"}, v, "", errors.Join(errs...))

	got := pod.Annotations[annotationPostCloneErrors]
	if got == "" {
		t.Fatal("post-clone-errors annotation not written")
	}
	if len(got) > lifecycleMessageMaxBytes {
		t.Errorf("annotation length %d exceeds cap %d", len(got), lifecycleMessageMaxBytes)
	}
	if len(got) != lifecycleMessageMaxBytes {
		t.Errorf("expected exact cap %d, got %d", lifecycleMessageMaxBytes, len(got))
	}
}

func TestRunPostCloneSetupSuccess(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	v := &vm.VM{ID: "vmid", NetworkConfigs: []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff", Network: &vm.NetworkInfo{IP: "10.0.0.5", Prefix: 24, Gateway: "10.0.0.1"}}}}

	p.runPostCloneSetup(t.Context(), pod, meta.VMSpec{Backend: "cloud-hypervisor", VMName: "vm"}, v, "", "create")

	if len(rt.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(rt.execCalls))
	}
	if pod.Annotations[annotationPostCloneState] != postCloneStateDone {
		t.Errorf("post-clone state = %q, want done", pod.Annotations[annotationPostCloneState])
	}
	if _, hasHint := pod.Annotations[annotationPostCloneHint]; hasHint {
		t.Errorf("hint annotation should not be set on success")
	}
}

func TestRunPostCloneSetupCancelSkipsFailedStateAndHint(t *testing.T) {
	// execErr=context.Canceled simulates the inner Exec returning canceled
	// after lifecycleCtx fires; pre-cancel the request ctx so loopCtx exits
	// after a single iteration.
	rt := &fakeRuntime{execErr: context.Canceled}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	v := &vm.VM{ID: "vmid", NetworkConfigs: []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff", Network: &vm.NetworkInfo{IP: "10.0.0.5", Prefix: 24, Gateway: "10.0.0.1"}}}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p.runPostCloneSetup(ctx, pod, meta.VMSpec{Backend: "cloud-hypervisor", VMName: "vm"}, v, "", "create")

	if pod.Annotations[annotationPostCloneState] == postCloneStateFailed {
		t.Errorf("cancellation must not write state=failed, got %q", pod.Annotations[annotationPostCloneState])
	}
	if _, hasHint := pod.Annotations[annotationPostCloneHint]; hasHint {
		t.Errorf("cancellation must not emit hint annotation")
	}
}

func TestIsClonedBoot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		fromDir  string
		forkFrom string
		mode     string
		want     bool
	}{
		{name: "mode=run, no fork, no dir", mode: "run", want: false},
		{name: "mode=clone", mode: "clone", want: true},
		{name: "mode=run + ForkFrom (sub-agent)", mode: "run", forkFrom: "main-vm", want: true},
		{name: "mode=run + clone-from-dir", mode: "run", fromDir: "/tmp/snap", want: true},
		{name: "mode empty defaults to clone", mode: "", want: true},
		{name: "mode=Run mixed-case", mode: "Run", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
			}
			if tc.fromDir != "" {
				pod.Annotations[meta.AnnotationCloneFromDir] = tc.fromDir
			}
			spec := meta.VMSpec{Mode: tc.mode, ForkFrom: tc.forkFrom}
			if got := isClonedBoot(pod, spec); got != tc.want {
				t.Errorf("isClonedBoot mode=%q forkFrom=%q fromDir=%q = %v, want %v",
					tc.mode, tc.forkFrom, tc.fromDir, got, tc.want)
			}
		})
	}
}

func TestRunPostCloneSetupNoOpSkipsState(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-0", Namespace: "ns"}}
	v := &vm.VM{ID: "vmid", NetworkConfigs: []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff"}}}

	p.runPostCloneSetup(t.Context(), pod, meta.VMSpec{Backend: "cloud-hypervisor"}, v, "", "create")

	if len(rt.execCalls) != 0 {
		t.Errorf("CH+OCI+DHCP no-op path should not call Exec, got %d calls", len(rt.execCalls))
	}
	if pod.Annotations[annotationPostCloneState] != "" {
		t.Errorf("no-op should leave post-clone-state unset, got %q", pod.Annotations[annotationPostCloneState])
	}
}

type failingSACDialer struct{}

func (failingSACDialer) Dial(context.Context, string) (guest.Session, error) {
	return nil, errors.New("simulated SAC dial failure")
}
