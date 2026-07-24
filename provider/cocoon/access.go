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

func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, _ string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	v := p.vmForPod(namespace, podName)
	if v == nil {
		return io.NopCloser(strings.NewReader("vk-cocoon: pod has no live VM\n")), nil
	}
	tail := opts.Tail
	if tail <= 0 {
		// Cap unbounded `kubectl logs` against long-running VMs.
		tail = 200
	}
	return p.Runtime.Logs(ctx, v.ID, tail)
}

func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, _ string, cmd []string, attach api.AttachIO) error {
	v := p.vmForPod(namespace, podName)
	if v == nil {
		return fmt.Errorf("pod %s/%s has no live VM", namespace, podName)
	}
	return p.Runtime.Exec(ctx, v.ID, cmd, nil, attach.Stdin(), attach.Stdout(), attach.Stderr())
}

func (p *Provider) AttachToContainer(_ context.Context, _, _, _ string, _ api.AttachIO) error {
	return errAttachNotImplemented
}

func (p *Provider) PortForward(_ context.Context, _, _ string, _ int32, _ io.ReadWriteCloser) error {
	return errPortForwardNotImplemented
}
