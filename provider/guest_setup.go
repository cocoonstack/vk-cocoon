package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
)

// runInitContainers executes init containers sequentially via SSH.
// Each init container's command is run inside the VM. If any fails,
// the pod is marked as failed. Called before starting main containers.
func (p *CocoonProvider) runInitContainers(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) error {
	if vm.skipSSH() {
		return nil
	}
	logger := log.WithFunc("provider.runInitContainers")
	pw := p.sshPass(vm)
	for i, ic := range pod.Spec.InitContainers {
		cmd := strings.Join(append(ic.Command, ic.Args...), " ")
		if cmd == "" {
			continue
		}
		logger.Infof(ctx, "initContainer[%d] %s/%s: running %q", i, pod.Namespace, pod.Name, cmd)
		out, err := p.guestExecutor().execSimple(ctx, vm, pw, cmd)
		if err != nil {
			logger.Errorf(ctx, err, "initContainer[%d] %s/%s failed: %s", i, pod.Namespace, pod.Name, out)
			return fmt.Errorf("init container %s failed: %w", ic.Name, err)
		}
		logger.Infof(ctx, "initContainer[%d] %s/%s: OK (%d bytes output)", i, pod.Namespace, pod.Name, len(out))
	}
	return nil
}

// installContainerServices creates a systemd service for each container spec.
// Container[0] is the primary; additional containers are sidecar services.
// Each service gets its own env file and command.
func (p *CocoonProvider) installContainerServices(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	if vm.skipSSH() || len(pod.Spec.Containers) <= 1 {
		return
	}
	logger := log.WithFunc("provider.installContainerServices")
	pw := p.sshPass(vm)
	for i, c := range pod.Spec.Containers {
		if i == 0 {
			continue
		}
		svcName := fmt.Sprintf("sidecar-%s", c.Name)
		cmd := strings.Join(append(c.Command, c.Args...), " ")
		if cmd == "" {
			continue
		}

		envContent := ""
		for _, e := range c.Env {
			if e.Value != "" {
				envContent += fmt.Sprintf("%s=%s\n", e.Name, e.Value)
			}
		}
		if envContent != "" {
			envPath := fmt.Sprintf("/opt/agent/sidecar-%s.env", c.Name)
			_ = p.guestExecutor().writeFile(ctx, vm, pw, envPath, []byte(envContent), 0o600)
		}

		unit := fmt.Sprintf(`[Unit]
Description=Sidecar: %s
After=network.target

[Service]
Type=simple
ExecStart=%s
EnvironmentFile=-/opt/agent/sidecar-%s.env
Restart=always
RestartSec=10
StandardOutput=append:/opt/agent/logs/%s.log
StandardError=append:/opt/agent/logs/%s.log

[Install]
WantedBy=multi-user.target
`, c.Name, cmd, c.Name, svcName, svcName)

		unitPath := fmt.Sprintf("/etc/systemd/system/%s.service", svcName)
		_ = p.guestExecutor().writeFile(ctx, vm, pw, unitPath, []byte(unit), 0o644)
		_, _ = p.guestExecutor().execSimple(ctx, vm, pw, fmt.Sprintf("systemctl daemon-reload && systemctl enable %s && systemctl start %s", svcName, svcName))
		logger.Infof(ctx, "%s/%s: sidecar %s started", pod.Namespace, pod.Name, svcName)
	}
}

// applySecurityContext maps pod/container security settings to VM config.
// For VMs, the main mapping is runAsUser -> create/switch Linux user.
// Capabilities, seccomp, apparmor are N/A (VM provides kernel-level isolation).
func (p *CocoonProvider) applySecurityContext(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	if vm.skipSSH() {
		return
	}
	logger := log.WithFunc("provider.applySecurityContext")
	sc := pod.Spec.SecurityContext
	if sc == nil && (len(pod.Spec.Containers) == 0 || pod.Spec.Containers[0].SecurityContext == nil) {
		return
	}

	pw := p.sshPass(vm)

	var uid *int64
	if sc != nil && sc.RunAsUser != nil {
		uid = sc.RunAsUser
	}
	if len(pod.Spec.Containers) > 0 {
		csc := pod.Spec.Containers[0].SecurityContext
		if csc != nil && csc.RunAsUser != nil {
			uid = csc.RunAsUser
		}
	}

	if uid != nil && *uid != 0 {
		username := fmt.Sprintf("app-%d", *uid)
		cmd := fmt.Sprintf("id -u %d >/dev/null 2>&1 || useradd -u %d -m %s", *uid, *uid, username)
		_, _ = p.guestExecutor().execSimple(ctx, vm, pw, cmd)
		logger.Infof(ctx, "%s/%s: runAsUser=%d (user=%s)", pod.Namespace, pod.Name, *uid, username)
	}

	if len(pod.Spec.Containers) > 0 {
		csc := pod.Spec.Containers[0].SecurityContext
		if csc != nil && csc.ReadOnlyRootFilesystem != nil && *csc.ReadOnlyRootFilesystem {
			logger.Warnf(ctx, "%s/%s: ReadOnlyRootFilesystem ignored (VM requires writable root)", pod.Namespace, pod.Name)
		}
	}
}

// injectDownwardAPIEnv adds pod metadata to the env file.
// These are the standard k8s downward API fields.
func (p *CocoonProvider) injectDownwardAPIEnv(pod *corev1.Pod, vm *CocoonVM) map[string]string {
	env := map[string]string{
		"POD_NAME":      pod.Name,
		"POD_NAMESPACE": pod.Namespace,
		"POD_IP":        vm.ip,
		"NODE_NAME":     pod.Spec.NodeName,
		"POD_UID":       string(pod.UID),
	}
	if sa := pod.Spec.ServiceAccountName; sa != "" {
		env["SERVICE_ACCOUNT_NAME"] = sa
	}
	for k, v := range pod.Labels {
		key := "POD_LABEL_" + strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(k, "/", "_"), ".", "_"))
		env[key] = v
	}
	return env
}

// injectSSHKey writes an SSH public key to the VM's authorized_keys.
// If annotation cocoon.cis/ssh-pubkey is set, inject it.
func (p *CocoonProvider) injectSSHKey(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	if vm.skipSSH() {
		return
	}
	logger := log.WithFunc("provider.injectSSHKey")
	pubkey := ann(pod, AnnSSHPubkey, "")
	if pubkey == "" {
		return
	}
	pw := p.sshPass(vm)
	cmd := fmt.Sprintf("mkdir -p /root/.ssh && echo '%s' >> /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys", pubkey)
	if _, err := p.guestExecutor().execSimple(ctx, vm, pw, cmd); err != nil {
		logger.Warnf(ctx, "%s/%s: %v", pod.Namespace, pod.Name, err)
	} else {
		logger.Infof(ctx, "%s/%s: SSH pubkey injected", pod.Namespace, pod.Name)
	}
}
