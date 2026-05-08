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
)

var (
	errAttachNotImplemented      = errors.New("vk-cocoon: AttachToContainer is not implemented")
	errPortForwardNotImplemented = errors.New("vk-cocoon: PortForward is not implemented")
)

// GetContainerLogs returns the per-VM hypervisor log via `cocoon vm logs`.
// Hypervisor-side log capture is OS-agnostic (CH stdio for direct-boot VMs,
// firecracker serial for FC), so Linux and Windows go through the same
// path. opts.Tail is forwarded to cocoon's --tail; default 200 keeps
// unbounded `kubectl logs` from streaming a long-running VM's full log.
func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, _ string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	v := p.vmForPod(namespace, podName)
	if v == nil {
		return io.NopCloser(strings.NewReader("vk-cocoon: pod has no live VM\n")), nil
	}
	tail := opts.Tail
	if tail <= 0 {
		tail = 200
	}
	return p.Runtime.Logs(ctx, v.ID, tail)
}

// RunInContainer is the kubectl exec entrypoint. Both Linux and Windows
// guests go through cocoon-agent over vsock (cocoonv2 5fdd77e + cocoon-agent
// v0.1.2 added Windows SCM service mode + viosock support).
func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, _ string, cmd []string, attach api.AttachIO) error {
	v := p.vmForPod(namespace, podName)
	if v == nil {
		return fmt.Errorf("pod %s/%s has no live VM", namespace, podName)
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
