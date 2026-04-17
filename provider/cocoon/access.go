package cocoon

import (
	"bytes"
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
	errSSHNotConfigured          = errors.New("vk-cocoon: SSH executor not configured")
)

// GetContainerLogs returns the guest's systemd journal (Linux) or a
// help-text pointing at RDP (Windows). Only opts.Tail is honored.
func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, _ string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	pod, err := p.GetPod(ctx, namespace, podName)
	if err != nil {
		return nil, err
	}
	v := p.vmForPod(namespace, podName)
	if v == nil || v.IP == "" {
		return io.NopCloser(strings.NewReader("vk-cocoon: pod has no live VM\n")), nil
	}

	if meta.IsWindowsPod(pod) {
		msg := fmt.Sprintf("vk-cocoon: kubectl logs is not supported on Windows guests; connect via RDP to %s\n", v.IP)
		return io.NopCloser(strings.NewReader(msg)), nil
	}

	if p.GuestSSH == nil {
		return io.NopCloser(strings.NewReader("vk-cocoon: SSH executor not configured\n")), nil
	}

	tail := opts.Tail
	if tail <= 0 {
		tail = 200
	}
	body, err := p.GuestSSH.FetchJournal(ctx, v.IP, tail)
	if err != nil {
		return nil, fmt.Errorf("fetch journal from %s: %w", v.IP, err)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// RunInContainer is the kubectl exec entrypoint (SSH for Linux, RDP help for Windows).
func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, _ string, cmd []string, attach api.AttachIO) error {
	v := p.vmForPod(namespace, podName)
	if v == nil || v.IP == "" {
		return fmt.Errorf("pod %s/%s has no live VM", namespace, podName)
	}
	pod, err := p.GetPod(ctx, namespace, podName)
	if err != nil {
		return err
	}
	if meta.IsWindowsPod(pod) {
		return p.GuestRDP.Run(ctx, v.IP, cmd, attach.Stdin(), attach.Stdout(), attach.Stderr())
	}
	if p.GuestSSH == nil {
		return errSSHNotConfigured
	}
	return p.GuestSSH.Run(ctx, v.IP, cmd, attach.Stdin(), attach.Stdout(), attach.Stderr())
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
