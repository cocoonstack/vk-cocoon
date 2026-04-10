package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
)

// GetContainerLogs returns the most recent log output from a pod's
// guest VM. For Linux guests this is the systemd journal; for
// Windows guests it returns a help-text message pointing the user
// at RDP.
func (p *CocoonProvider) GetContainerLogs(ctx context.Context, namespace, podName, _ string, _ ContainerLogOpts) (io.ReadCloser, error) {
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

	body, err := p.GuestSSH.FetchJournal(ctx, v.IP, 200)
	if err != nil {
		return nil, fmt.Errorf("fetch journal from %s: %w", v.IP, err)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// RunInContainer is the kubectl exec entrypoint. The Linux path
// goes through the SSH executor; Windows returns the help text.
func (p *CocoonProvider) RunInContainer(ctx context.Context, namespace, podName, _ string, cmd []string, attach AttachIO) error {
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
		return errNotImplemented
	}
	return p.GuestSSH.Run(ctx, v.IP, cmd, attach.Stdin(), attach.Stdout(), attach.Stderr())
}

// ContainerLogOpts is the small subset of v-k's
// api.ContainerLogOpts vk-cocoon needs. Decoupling here keeps the
// import surface narrow during the rewrite period; the production
// adapter to v-k's interface lives in main.go (a future commit
// wires it).
type ContainerLogOpts struct {
	Tail         int
	LimitBytes   int
	Timestamps   bool
	Follow       bool
	Previous     bool
	SinceSeconds int
}

// AttachIO mirrors v-k's api.AttachIO so RunInContainer can stream
// stdin / stdout / stderr through the SSH session.
type AttachIO interface {
	Stdin() io.Reader
	Stdout() io.WriteCloser
	Stderr() io.WriteCloser
	TTY() bool
}

// isWindowsPod inspects the pod's annotations to decide whether the
// guest is Windows. The operator writes meta.AnnotationOS via
// meta.VMSpec.Apply.
func isWindowsPod(pod interface{ GetAnnotations() map[string]string }) bool {
	return strings.EqualFold(pod.GetAnnotations()["cocoonset.cocoonstack.io/os"], "windows")
}
