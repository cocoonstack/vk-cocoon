// Package provider — file injection helpers for VM volumes and env vars.
//
// In containers, kubelet mounts ConfigMaps/Secrets as tmpfs volumes.
// For VMs, we SSH-write the files after boot. The VK framework resolves
// all configMapKeyRef/secretKeyRef/fieldRef in pod.Spec.Containers[0].Env
// before calling CreatePod, so we get plain Name/Value pairs.
package provider

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// Annotation keys for injection config.
const (
	AnnEnvFile     = "cocoon.cis/env-file"     // target path for env file (default: /opt/agent/pod.env)
	AnnServiceName = "cocoon.cis/service-name" // systemd service to restart on env change
)

var (
	sshReadyTimeout      = 45 * time.Second
	sshReadyPollInterval = 2 * time.Second
	sshReadyProbe        = func(ctx context.Context, vm *CocoonVM, password string) error {
		_, err := sshExecSimple(ctx, vm, password, "true")
		return err
	}
)

// sshWriteFile writes data to a file on the VM via SSH stdin pipe.
// Creates parent directories. Does NOT use SCP (avoids binary dependency).
func sshWriteFile(ctx context.Context, vm *CocoonVM, password, path string, data []byte, mode int) error {
	dir := path[:strings.LastIndex(path, "/")]
	// Pass the script as a single SSH remote command (not via bash -c which
	// has quoting issues). SSH concatenates all trailing args as the command.
	cmd := exec.CommandContext(ctx, "sshpass", "-p", password, //nolint:gosec // SSH args from pod spec
		"ssh", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR", "-o", "ConnectTimeout=5",
		fmt.Sprintf("root@%s", vm.ip),
		fmt.Sprintf("mkdir -p %s && cat > %s && chmod %04o %s", dir, path, mode, path))
	cmd.Stdin = strings.NewReader(string(data))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sshWriteFile %s: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sshExecSimple runs a command on the VM and returns combined output.
func sshExecSimple(ctx context.Context, vm *CocoonVM, password, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sshpass", "-p", password, //nolint:gosec // SSH args from pod spec
		"ssh", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR", "-o", "ConnectTimeout=5",
		fmt.Sprintf("root@%s", vm.ip), command)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func waitForSSH(ctx context.Context, vm *CocoonVM, password string, timeout time.Duration) error {
	if vm == nil || vm.ip == "" {
		return fmt.Errorf("vm has no IP")
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lastErr = sshReadyProbe(ctx, vm, password)
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ssh not ready after %s: %w", timeout, lastErr)
		}
		time.Sleep(sshReadyPollInterval)
	}
}

// ---------- Environment variable injection ----------

// injectEnvVars writes pod.Spec.Containers[0].Env as a systemd-compatible env file.
// The VK framework has already resolved configMapKeyRef/secretKeyRef/fieldRef.
// Returns a content hash for change detection.
func (p *CocoonProvider) injectEnvVars(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) (string, error) {
	if vm.skipSSH() {
		return "", nil
	}
	envs := []string{}
	if len(pod.Spec.Containers) > 0 {
		for _, e := range pod.Spec.Containers[0].Env {
			if e.Value != "" {
				envs = append(envs, fmt.Sprintf("%s=%s", e.Name, e.Value))
			}
		}
	}
	// #22: Add DownwardAPI env vars (pod metadata)
	for k, v := range p.injectDownwardAPIEnv(pod, vm) {
		envs = append(envs, fmt.Sprintf("%s=%s", k, v))
	}
	if len(envs) == 0 {
		return "", nil
	}
	sort.Strings(envs)
	content := strings.Join(envs, "\n") + "\n"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))

	target := ann(pod, AnnEnvFile, "/opt/agent/pod.env")
	pw := p.sshPass(vm)
	if err := sshWriteFile(ctx, vm, pw, target, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("injectEnvVars: %w", err)
	}
	klog.Infof("injectEnvVars %s/%s: wrote %d vars to %s", pod.Namespace, pod.Name, len(envs), target)
	return hash, nil
}

// ---------- ConfigMap/Secret volume injection ----------

// injectVolumes writes ConfigMap/Secret volume data as files in the VM.
// Returns a content hash for change detection.
func (p *CocoonProvider) injectVolumes(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) (string, error) {
	if vm.skipSSH() {
		return "", nil
	}
	if p.configMapLister == nil && p.secretLister == nil {
		return "", nil
	}

	// Build mountPath lookup from container volumeMounts
	mounts := map[string]string{} // volumeName -> mountPath
	if len(pod.Spec.Containers) > 0 {
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			mounts[m.Name] = m.MountPath
		}
	}

	pw := p.sshPass(vm)
	var allContent strings.Builder
	fileCount := 0

	for _, vol := range pod.Spec.Volumes {
		mountPath, ok := mounts[vol.Name]
		if !ok {
			continue
		}

		var data map[string]string
		var binData map[string][]byte
		mode := 0o644

		switch {
		case vol.ConfigMap != nil && p.configMapLister != nil:
			cm, err := p.configMapLister.ConfigMaps(pod.Namespace).Get(vol.ConfigMap.Name)
			if err != nil {
				if vol.ConfigMap.Optional != nil && *vol.ConfigMap.Optional {
					continue
				}
				return "", fmt.Errorf("get configmap %s: %w", vol.ConfigMap.Name, err)
			}
			data = cm.Data
			binData = cm.BinaryData
			if vol.ConfigMap.DefaultMode != nil {
				mode = int(*vol.ConfigMap.DefaultMode)
			}
		case vol.Secret != nil && p.secretLister != nil:
			sec, err := p.secretLister.Secrets(pod.Namespace).Get(vol.Secret.SecretName)
			if err != nil {
				if vol.Secret.Optional != nil && *vol.Secret.Optional {
					continue
				}
				return "", fmt.Errorf("get secret %s: %w", vol.Secret.SecretName, err)
			}
			// Secret.Data is map[string][]byte
			binData = sec.Data
			if vol.Secret.DefaultMode != nil {
				mode = int(*vol.Secret.DefaultMode)
			}
		default:
			continue // skip unsupported volume types (emptyDir, hostPath, etc.)
		}

		// Write string data
		for key, val := range data {
			path := mountPath + "/" + key
			if err := sshWriteFile(ctx, vm, pw, path, []byte(val), mode); err != nil {
				return "", err
			}
			allContent.WriteString(key + "=" + val + "\n")
			fileCount++
		}
		// Write binary data
		for key, val := range binData {
			path := mountPath + "/" + key
			if err := sshWriteFile(ctx, vm, pw, path, val, mode); err != nil {
				return "", err
			}
			allContent.Write(val)
			fileCount++
		}
	}

	hash := ""
	if fileCount > 0 {
		hash = fmt.Sprintf("%x", sha256.Sum256([]byte(allContent.String())))
		klog.Infof("injectVolumes %s/%s: wrote %d files", pod.Namespace, pod.Name, fileCount)
	}
	return hash, nil
}

// postBootInject runs all injections and lifecycle setup after VM boot.
// Order: security → init containers → volumes → env → sidecars → DNS → SSH key.
func (p *CocoonProvider) postBootInject(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	if vm.os == osWindows {
		return
	}
	key := podKey(pod.Namespace, pod.Name)
	pw := p.sshPass(vm)

	if err := waitForSSH(ctx, vm, pw, sshReadyTimeout); err != nil {
		klog.Warningf("postBootInject %s: SSH not ready: %v", key, err)
		return
	}

	// #14: Security context (create user if runAsUser specified)
	p.applySecurityContext(ctx, pod, vm)

	// #16: SSH key injection
	p.injectSSHKey(ctx, pod, vm)

	// #13: Init containers (must complete before main service starts)
	if err := p.runInitContainers(ctx, pod, vm); err != nil {
		klog.Errorf("postBootInject %s: init containers failed: %v", key, err)
		return // don't proceed if init fails
	}

	// #11/#21: Volumes (emptyDir, hostPath, projected/downwardAPI)
	p.setupVolumes(ctx, pod, vm)

	// #5: ConfigMap/Secret volumes
	if volHash, err := p.injectVolumes(ctx, pod, vm); err != nil {
		klog.Warningf("postBootInject %s: volume injection failed: %v", key, err)
	} else if volHash != "" {
		p.mu.Lock()
		p.injectHashes[key+"/vol"] = volHash
		p.mu.Unlock()
	}

	// #6/#22: Env vars + DownwardAPI metadata
	if envHash, err := p.injectEnvVars(ctx, pod, vm); err != nil {
		klog.Warningf("postBootInject %s: env injection failed: %v", key, err)
	} else if envHash != "" {
		p.mu.Lock()
		p.injectHashes[key+"/env"] = envHash
		p.mu.Unlock()
	}

	// #12: Multi-container → sidecar systemd services
	p.installContainerServices(ctx, pod, vm)

	// #17: Pod DNS entry
	addPodDNS(pod.Name, pod.Namespace, vm.ip)

	// #24: Resource enforcement via CH API
	p.enforceResources(ctx, pod, vm)
}
