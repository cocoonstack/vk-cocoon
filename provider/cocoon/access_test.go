package cocoon

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/probes"
	"github.com/cocoonstack/vk-cocoon/vm"
)

type stubAttachIO struct {
	stdin  io.Reader
	stdout *bufferCloser
	stderr *bufferCloser
}

type bufferCloser struct{ bytes.Buffer }

func (*bufferCloser) Close() error { return nil }

func newStubAttachIO(stdin string) *stubAttachIO {
	return &stubAttachIO{
		stdin:  strings.NewReader(stdin),
		stdout: &bufferCloser{},
		stderr: &bufferCloser{},
	}
}

func (s *stubAttachIO) Stdin() io.Reader          { return s.stdin }
func (s *stubAttachIO) Stdout() io.WriteCloser    { return s.stdout }
func (s *stubAttachIO) Stderr() io.WriteCloser    { return s.stderr }
func (*stubAttachIO) TTY() bool                   { return false }
func (*stubAttachIO) Resize() <-chan api.TermSize { return nil }

func newRunningPod(t *testing.T, p *Provider, name, vmID, ip string, windows bool) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
	spec := meta.VMSpec{VMName: "vm-" + name, Managed: true}
	if windows {
		spec.OS = "windows"
	}
	spec.Apply(pod)
	v := &vm.VM{ID: vmID, Name: spec.VMName, IP: ip, State: vm.StateRunning}
	p.trackPod(pod, v)
	return pod
}

func TestRunInContainerLinuxRoutesToRuntimeExec(t *testing.T) {
	rt := &fakeRuntime{execStdout: "hello\n", execExitCode: 0}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newRunningPod(t, p, "demo-0", "vmid-1", "10.0.0.5", false)
	aio := newStubAttachIO("stdin-bytes")
	if err := p.RunInContainer(t.Context(), pod.Namespace, pod.Name, "", []string{"echo", "hello"}, aio); err != nil {
		t.Fatalf("RunInContainer: %v", err)
	}
	if len(rt.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(rt.execCalls))
	}
	got := rt.execCalls[0]
	if got.vmID != "vmid-1" {
		t.Errorf("vmID = %q, want vmid-1", got.vmID)
	}
	if !reflect.DeepEqual(got.argv, []string{"echo", "hello"}) {
		t.Errorf("argv = %v, want [echo hello]", got.argv)
	}
	if got.stdin != "stdin-bytes" {
		t.Errorf("stdin = %q, want stdin-bytes", got.stdin)
	}
	if aio.stdout.String() != "hello\n" {
		t.Errorf("stdout = %q, want \"hello\\n\"", aio.stdout.String())
	}
}

func TestRunInContainerSurfacesNonZeroExit(t *testing.T) {
	rt := &fakeRuntime{execExitCode: 7}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newRunningPod(t, p, "demo-0", "vmid-1", "10.0.0.5", false)
	err := p.RunInContainer(t.Context(), pod.Namespace, pod.Name, "", []string{"sh", "-c", "exit 7"}, newStubAttachIO(""))
	if err == nil {
		t.Fatal("expected non-zero exit to surface as error")
	}
	// vk's RemoteCommand handler does the same type assertion to map child
	// exit codes onto the kubectl wire protocol; if this assertion fails
	// users get a 500 instead of the right exit status.
	var exitErr utilexec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected utilexec.ExitError, got %T: %v", err, err)
	}
	if !exitErr.Exited() || exitErr.ExitStatus() != 7 {
		t.Errorf("ExitStatus = %d (Exited=%v), want 7 (true)", exitErr.ExitStatus(), exitErr.Exited())
	}
}

func TestRunInContainerSurfacesRuntimeError(t *testing.T) {
	rt := &fakeRuntime{execErr: errors.New("dial agent: connection refused")}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())

	pod := newRunningPod(t, p, "demo-0", "vmid-1", "10.0.0.5", false)
	err := p.RunInContainer(t.Context(), pod.Namespace, pod.Name, "", []string{"echo", "hi"}, newStubAttachIO(""))
	if err == nil {
		t.Fatal("expected runtime error to surface")
	}
	if !strings.Contains(err.Error(), "dial agent") {
		t.Errorf("error chain missing root cause: %v", err)
	}
}

func TestRunInContainerNoLiveVMRejected(t *testing.T) {
	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Probes = probes.NewManager(t.Context())
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ghost-0", Namespace: "ns"}}
	err := p.RunInContainer(t.Context(), pod.Namespace, pod.Name, "", []string{"echo", "hi"}, newStubAttachIO(""))
	if err == nil || !strings.Contains(err.Error(), "no live VM") {
		t.Fatalf("expected 'no live VM' error, got %v", err)
	}
	if len(rt.execCalls) != 0 {
		t.Errorf("Runtime.Exec should not be called when VM is missing, got %d calls", len(rt.execCalls))
	}
}
