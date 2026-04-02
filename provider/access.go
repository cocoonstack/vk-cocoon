package provider

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
)

func (p *CocoonProvider) GetContainerLogs(ctx context.Context, ns, podName, container string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	log.WithFunc("provider.GetContainerLogs").Infof(ctx, "%s/%s container=%s tail=%d follow=%v previous=%v timestamps=%v since=%d",
		ns, podName, container, opts.Tail, opts.Follow, opts.Previous, opts.Timestamps, opts.SinceSeconds)

	if opts.Previous {
		return io.NopCloser(strings.NewReader("")), nil
	}

	vm := p.getVM(ns, podName)
	if vm == nil || vm.ip == "" {
		return io.NopCloser(strings.NewReader("pod not found or no IP assigned\n")), nil
	}

	if vm.os == osWindows {
		msg := fmt.Sprintf("Windows VM %s (IP: %s) - use RDP (port 3389) for access.\n"+
			"  kubectl port-forward %s 3389:3389 -n %s\n"+
			"  Then connect with Remote Desktop to localhost:3389\n", vm.vmName, vm.ip, podName, ns)
		return io.NopCloser(strings.NewReader(msg)), nil
	}

	lines := "500"
	if opts.Tail > 0 {
		lines = strconv.Itoa(opts.Tail)
	}

	journalctlArgs := []string{"journalctl", "--no-pager", "-n", lines}
	if opts.Timestamps {
		journalctlArgs = append(journalctlArgs, "--output=short-iso")
	}
	if opts.Follow {
		journalctlArgs = append(journalctlArgs, "-f")
	}
	if opts.SinceSeconds > 0 {
		journalctlArgs = append(journalctlArgs, fmt.Sprintf("--since=%d seconds ago", opts.SinceSeconds))
	}
	if !opts.SinceTime.IsZero() {
		journalctlArgs = append(journalctlArgs, fmt.Sprintf("--since=%s", opts.SinceTime.Format("2006-01-02 15:04:05")))
	}

	cmd := p.guestExecutor().command(ctx, vm, p.sshPass(vm), false, journalctlArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return io.NopCloser(strings.NewReader(fmt.Sprintf("pipe error: %v\n", err))), nil
	}
	if err := cmd.Start(); err != nil {
		return io.NopCloser(strings.NewReader(fmt.Sprintf("ssh error: %v\n", err))), nil
	}
	return stdout, nil
}

func (p *CocoonProvider) RunInContainer(ctx context.Context, ns, podName, container string, cmd []string, attach api.AttachIO) error {
	vm := p.getVM(ns, podName)
	if vm == nil || vm.ip == "" {
		return fmt.Errorf("pod %s/%s not found or no IP", ns, podName)
	}

	log.WithFunc("provider.RunInContainer").Infof(ctx, "%s/%s cmd=%v tty=%v", ns, podName, cmd, attach.TTY())

	if vm.os == osWindows {
		msg := fmt.Sprintf("exec not supported on Windows VM. Use RDP:\n  kubectl port-forward %s 3389:3389 -n %s\n", podName, ns)
		if attach.Stdout() != nil {
			_, _ = io.WriteString(attach.Stdout(), msg)
		}
		return fmt.Errorf("exec not supported on Windows VM (use RDP port 3389)")
	}

	remoteArgs := []string{}
	if len(cmd) > 0 {
		remoteArgs = append(remoteArgs, "--", shellQuoteJoin(cmd))
	}

	sshCmd := p.guestExecutor().command(ctx, vm, p.sshPass(vm), attach.TTY(), remoteArgs...)
	stdinPipe, _ := sshCmd.StdinPipe()
	stdoutPipe, _ := sshCmd.StdoutPipe()
	stderrPipe, _ := sshCmd.StderrPipe()

	if err := sshCmd.Start(); err != nil {
		return fmt.Errorf("ssh start: %w", err)
	}

	done := make(chan struct{})

	if attach.Stdin() != nil {
		go func() {
			_, _ = io.Copy(stdinPipe, attach.Stdin())
			_ = stdinPipe.Close()
		}()
	}
	if attach.Stdout() != nil {
		go func() {
			_, _ = io.Copy(attach.Stdout(), stdoutPipe)
			done <- struct{}{}
		}()
	}
	if attach.Stderr() != nil {
		go func() {
			_, _ = io.Copy(attach.Stderr(), stderrPipe)
		}()
	}
	if attach.TTY() && attach.Resize() != nil {
		go func() {
			for size := range attach.Resize() {
				_, _ = fmt.Fprintf(stdinPipe, "stty rows %d cols %d\n", size.Height, size.Width)
			}
		}()
	}

	if attach.Stdout() != nil {
		<-done
	}
	return sshCmd.Wait()
}

func (p *CocoonProvider) AttachToContainer(ctx context.Context, ns, podName, container string, attach api.AttachIO) error {
	return p.RunInContainer(ctx, ns, podName, container, []string{"bash", "-l"}, attach)
}

func (p *CocoonProvider) PortForward(ctx context.Context, ns, podName string, port int32, stream io.ReadWriteCloser) error {
	vm := p.getVM(ns, podName)
	if vm == nil || vm.ip == "" {
		return fmt.Errorf("pod %s/%s not found or no IP", ns, podName)
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", vm.ip, port), 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect to %s:%d: %w", vm.ip, port, err)
	}
	defer func() { _ = conn.Close() }()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(conn, stream); done <- struct{}{} }()
	go func() { _, _ = io.Copy(stream, conn); done <- struct{}{} }()
	<-done
	return nil
}
