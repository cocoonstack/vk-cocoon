package provider

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
)

const (
	AnnEnvFile     = "cocoon.cis/env-file"
	AnnServiceName = "cocoon.cis/service-name"
)

var (
	sshReadyTimeout      = 45 * time.Second
	sshReadyPollInterval = 2 * time.Second
	sshReadyProbe        = func(ctx context.Context, vm *CocoonVM, password string) error {
		_, err := guestExecutor{}.execSimple(ctx, vm, password, "true")
		return err //nolint:wrapcheck // probe returns the SSH error directly
	}
)

func (p *CocoonProvider) injectEnvVars(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) (string, error) {
	if vm.skipSSH() {
		return "", nil
	}
	var envs []string
	if len(pod.Spec.Containers) > 0 {
		for _, e := range pod.Spec.Containers[0].Env {
			if e.Value != "" {
				envs = append(envs, fmt.Sprintf("%s=%s", e.Name, e.Value))
			}
		}
	}
	for k, v := range p.injectDownwardAPIEnv(pod, vm) {
		envs = append(envs, fmt.Sprintf("%s=%s", k, v))
	}
	if len(envs) == 0 {
		return "", nil
	}
	slices.Sort(envs)
	content := strings.Join(envs, "\n") + "\n"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))

	target := ann(pod, AnnEnvFile, "/opt/agent/pod.env")
	pw := p.sshPass(vm)
	if err := p.guestExecutor().writeFile(ctx, vm, pw, target, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("inject env vars: %w", err)
	}
	log.WithFunc("provider.injectEnvVars").Infof(ctx, "%s/%s: wrote %d vars to %s", pod.Namespace, pod.Name, len(envs), target)
	return hash, nil
}

func (p *CocoonProvider) injectVolumes(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) (string, error) {
	if vm.skipSSH() {
		return "", nil
	}
	if p.configMapLister == nil && p.secretLister == nil {
		return "", nil
	}

	mounts := map[string]string{}
	if len(pod.Spec.Containers) > 0 {
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			mounts[m.Name] = m.MountPath
		}
	}

	pw := p.sshPass(vm)
	var allContent strings.Builder
	var batch []batchFile

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
			binData = sec.Data
			if vol.Secret.DefaultMode != nil {
				mode = int(*vol.Secret.DefaultMode)
			}
		default:
			continue
		}

		for key, val := range data {
			path := mountPath + "/" + key
			batch = append(batch, batchFile{path: path, data: []byte(val), mode: mode})
			allContent.WriteString(key + "=" + val + "\n")
		}
		for key, val := range binData {
			path := mountPath + "/" + key
			batch = append(batch, batchFile{path: path, data: val, mode: mode})
			allContent.Write(val)
		}
	}

	if len(batch) > 0 {
		if err := p.guestExecutor().writeFilesBatch(ctx, vm, pw, batch); err != nil {
			return "", fmt.Errorf("inject volumes: %w", err)
		}
	}

	hash := ""
	if len(batch) > 0 {
		hash = fmt.Sprintf("%x", sha256.Sum256([]byte(allContent.String())))
		log.WithFunc("provider.injectVolumes").Infof(ctx, "%s/%s: wrote %d files", pod.Namespace, pod.Name, len(batch))
	}
	return hash, nil
}

func (p *CocoonProvider) postBootInject(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	if vm.os == osWindows {
		return
	}
	logger := log.WithFunc("provider.postBootInject")
	key := podKey(pod.Namespace, pod.Name)
	pw := p.sshPass(vm)

	if err := p.guestExecutor().waitForSSH(ctx, vm, pw, sshReadyTimeout); err != nil {
		logger.Warnf(ctx, "%s: SSH not ready: %v", key, err)
		return
	}

	p.applySecurityContext(ctx, pod, vm)
	p.injectSSHKey(ctx, pod, vm)

	if err := p.runInitContainers(ctx, pod, vm); err != nil {
		logger.Errorf(ctx, err, "%s: init containers failed", key)
		return
	}

	p.setupVolumes(ctx, pod, vm)

	if volHash, err := p.injectVolumes(ctx, pod, vm); err != nil {
		logger.Warnf(ctx, "%s: volume injection failed: %v", key, err)
	} else if volHash != "" {
		p.mu.Lock()
		p.injectHashes[key+"/vol"] = volHash
		p.mu.Unlock()
	}

	if envHash, err := p.injectEnvVars(ctx, pod, vm); err != nil {
		logger.Warnf(ctx, "%s: env injection failed: %v", key, err)
	} else if envHash != "" {
		p.mu.Lock()
		p.injectHashes[key+"/env"] = envHash
		p.mu.Unlock()
	}

	p.installContainerServices(ctx, pod, vm)
	addPodDNS(pod.Name, pod.Namespace, vm.ip)
	p.enforceResources(ctx, pod, vm)
}
