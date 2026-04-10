package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	cocoonv1alpha1 "github.com/cocoonstack/cocoon-common/apis/v1alpha1"
	"github.com/cocoonstack/cocoon-common/meta"
)

// Stub errors returned by the not-yet-implemented kubelet surface.
// virtual-kubelet's handler turns these into 500s; kubectl still
// gets a readable message.
var (
	errAttachNotImplemented      = errors.New("vk-cocoon: AttachToContainer is not implemented")
	errPortForwardNotImplemented = errors.New("vk-cocoon: PortForward is not implemented")
	errSSHNotConfigured          = errors.New("vk-cocoon: SSH executor not configured")
)

// GetContainerLogs returns the most recent log output from a pod's
// guest VM. For Linux guests this is the systemd journal; for
// Windows guests it returns a help-text message pointing the user
// at RDP. The opts struct is currently consulted only for Tail —
// everything else (Follow, SinceTime, Previous, etc.) is ignored
// and a fresh snapshot is returned.
func (p *CocoonProvider) GetContainerLogs(ctx context.Context, namespace, podName, _ string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	pod, err := p.GetPod(ctx, namespace, podName)
	if err != nil {
		return nil, err
	}
	v := p.vmForPod(namespace, podName)
	if v == nil || v.IP == "" {
		return io.NopCloser(strings.NewReader("vk-cocoon: pod has no live VM\n")), nil
	}

	if isWindowsPod(pod) {
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

// RunInContainer is the kubectl exec entrypoint. The Linux path
// goes through the SSH executor; Windows returns the help text.
func (p *CocoonProvider) RunInContainer(ctx context.Context, namespace, podName, _ string, cmd []string, attach api.AttachIO) error {
	v := p.vmForPod(namespace, podName)
	if v == nil || v.IP == "" {
		return fmt.Errorf("pod %s/%s has no live VM", namespace, podName)
	}
	pod, err := p.GetPod(ctx, namespace, podName)
	if err != nil {
		return err
	}
	if isWindowsPod(pod) {
		return p.GuestRDP.Run(ctx, v.IP, cmd, attach.Stdin(), attach.Stdout(), attach.Stderr())
	}
	if p.GuestSSH == nil {
		return errSSHNotConfigured
	}
	return p.GuestSSH.Run(ctx, v.IP, cmd, attach.Stdin(), attach.Stdout(), attach.Stderr())
}

// AttachToContainer is not yet implemented. virtual-kubelet turns
// the returned error into a 500 so the kubectl client gets a
// readable message.
func (p *CocoonProvider) AttachToContainer(_ context.Context, _, _, _ string, _ api.AttachIO) error {
	return errAttachNotImplemented
}

// PortForward is not yet implemented.
func (p *CocoonProvider) PortForward(_ context.Context, _, _ string, _ int32, _ io.ReadWriteCloser) error {
	return errPortForwardNotImplemented
}

// GetStatsSummary returns a minimal kubelet stats summary. The
// cocoon runtime tracks its own hypervisor stats elsewhere; this
// stub keeps the kubelet /stats/summary endpoint answering.
func (p *CocoonProvider) GetStatsSummary(_ context.Context) (*statsv1alpha1.Summary, error) {
	return &statsv1alpha1.Summary{
		Node: statsv1alpha1.NodeStats{NodeName: p.NodeName},
	}, nil
}

// GetMetricsResource returns an empty Prometheus metric family
// slice. vk-cocoon's real prometheus collectors live on the
// separate metrics listener managed in main.go.
func (p *CocoonProvider) GetMetricsResource(_ context.Context) ([]*dto.MetricFamily, error) {
	return nil, nil
}

// isWindowsPod reports whether the pod's VMSpec asks for a Windows
// guest. The operator writes meta.AnnotationOS via meta.VMSpec.Apply.
func isWindowsPod(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	return strings.EqualFold(pod.Annotations[meta.AnnotationOS], string(cocoonv1alpha1.OSWindows))
}
