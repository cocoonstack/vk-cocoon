// Package cocoon implements the virtual-kubelet provider for cocoon MicroVMs.
// It owns pod lifecycle (create / delete / update), exec / logs over the
// cocoon-agent vsock channel, status reporting, and stats.
package cocoon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"

	"github.com/cocoonstack/cocoon-common/meta"
)

var (
	errAttachNotImplemented      = errors.New("vk-cocoon: AttachToContainer is not implemented")
	errPortForwardNotImplemented = errors.New("vk-cocoon: PortForward is not implemented")
)

// GetContainerLogs returns the per-VM hypervisor log via `cocoon vm logs`.
// For OCI direct-boot VMs the guest console is wired to CH stdio so the
// file approximates a container's stdout. opts.Tail is forwarded to
// cocoon's --tail; default 200 keeps unbounded `kubectl logs` from
// streaming a long-running VM's full log. Windows gets an RDP help stub.
func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, _ string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	pod, err := p.GetPod(ctx, namespace, podName)
	if err != nil {
		return nil, err
	}
	v := p.vmForPod(namespace, podName)
	if v == nil {
		return io.NopCloser(strings.NewReader("vk-cocoon: pod has no live VM\n")), nil
	}
	if meta.IsWindowsPod(pod) {
		msg := fmt.Sprintf("vk-cocoon: kubectl logs is not supported on Windows guests; connect via RDP to %s\n", v.IP)
		return io.NopCloser(strings.NewReader(msg)), nil
	}
	tail := opts.Tail
	if tail <= 0 {
		tail = 200
	}
	return p.Runtime.Logs(ctx, v.ID, tail)
}

// RunInContainer is the kubectl exec entrypoint. Linux pods go through
// cocoon-agent over vsock; Windows pods get an RDP help-text stub until
// cocoon-agent grows Windows support.
func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, _ string, cmd []string, attach api.AttachIO) error {
	v := p.vmForPod(namespace, podName)
	if v == nil {
		return fmt.Errorf("pod %s/%s has no live VM", namespace, podName)
	}
	pod, err := p.GetPod(ctx, namespace, podName)
	if err != nil {
		return err
	}
	if meta.IsWindowsPod(pod) {
		return p.GuestRDP.Run(ctx, v.IP, cmd, attach.Stdin(), attach.Stdout(), attach.Stderr())
	}
	return p.Runtime.Exec(ctx, v.ID, cmd, nil, attach.Stdin(), attach.Stdout(), attach.Stderr())
}

// AttachToContainer is not implemented.
func (p *Provider) AttachToContainer(_ context.Context, _, _, _ string, _ api.AttachIO) error {
	return errAttachNotImplemented
}

// PortForward is not implemented.
func (p *Provider) PortForward(_ context.Context, _, _ string, _ int32, _ io.ReadWriteCloser) error {
	return errPortForwardNotImplemented
}

// GetStatsSummary and GetMetricsResource are implemented in stats.go.
