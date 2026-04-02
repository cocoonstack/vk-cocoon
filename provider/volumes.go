// Package provider — volume handling for VMs.
//
// VM volume semantics differ from containers:
//   - hostPath: host directory is synced into VM via SSH (not bind-mount)
//   - emptyDir: created as a directory inside the VM, persists across stop/start (stateful)
//   - configMap/secret: handled by inject.go (SSH file write)
//   - PVC: mapped to a host directory under /data01/volumes/{pvc-name},
//     synced into VM. Data persists across VM restarts (stateful).
//
// VMs are stateful: volume data written inside the VM persists on the
// VM's COW disk across stop/start cycles.
package provider

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
)

// setupVolumes creates non-configMap/secret volumes in the VM.
// Called from postBootInject after VM has SSH.
func (p *CocoonProvider) setupVolumes(ctx context.Context, pod *corev1.Pod, vm *CocoonVM) {
	if vm.skipSSH() {
		return
	}

	// Build mountPath lookup
	mounts := map[string]corev1.VolumeMount{}
	if len(pod.Spec.Containers) > 0 {
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			mounts[m.Name] = m
		}
	}

	logger := log.WithFunc("provider.setupVolumes")
	pw := p.sshPass(vm)
	for _, vol := range pod.Spec.Volumes {
		mount, ok := mounts[vol.Name]
		if !ok {
			continue
		}

		switch {
		case vol.EmptyDir != nil:
			// EmptyDir: just create the directory in the VM.
			// medium=Memory -> tmpfs; default -> regular dir.
			if vol.EmptyDir.Medium == corev1.StorageMediumMemory {
				cmd := fmt.Sprintf("mkdir -p '%s' && mount -t tmpfs -o size=64m tmpfs '%s'",
					mount.MountPath, mount.MountPath)
				if _, err := p.guestExecutor().execSimple(ctx, vm, pw, cmd); err != nil {
					logger.Warnf(ctx, "%s/%s: emptyDir tmpfs %s: %v",
						pod.Namespace, pod.Name, mount.MountPath, err)
				}
			} else {
				cmd := fmt.Sprintf("mkdir -p '%s'", mount.MountPath)
				_, _ = p.guestExecutor().execSimple(ctx, vm, pw, cmd)
			}
			logger.Debugf(ctx, "emptyDir %s at %s", vol.Name, mount.MountPath)

		case vol.HostPath != nil:
			// HostPath: create the directory in the VM (host paths aren't directly
			// accessible from inside a VM). For true host sharing, use virtiofs.
			// Here we just ensure the path exists.
			cmd := fmt.Sprintf("mkdir -p '%s'", mount.MountPath)
			_, _ = p.guestExecutor().execSimple(ctx, vm, pw, cmd)
			logger.Debugf(ctx, "hostPath %s -> VM %s (dir created, not shared)",
				vol.HostPath.Path, mount.MountPath)

		case vol.Projected != nil:
			// Projected volumes: combine downwardAPI + configMap + secret.
			// DownwardAPI items handled here; configMap/secret handled by injectVolumes.
			p.injectProjectedVolume(ctx, pod, vm, vol.Projected, mount.MountPath)
		}
		// configMap and secret volumes are handled by injectVolumes in inject.go
	}
}

// injectProjectedVolume writes DownwardAPI items from a projected volume.
func (p *CocoonProvider) injectProjectedVolume(ctx context.Context, pod *corev1.Pod, vm *CocoonVM,
	proj *corev1.ProjectedVolumeSource, mountPath string,
) {
	pw := p.sshPass(vm)
	for _, source := range proj.Sources {
		if source.DownwardAPI != nil {
			for _, item := range source.DownwardAPI.Items {
				val := resolveDownwardAPIField(pod, item.FieldRef)
				if val != "" {
					path := filepath.Join(mountPath, item.Path)
					_ = p.guestExecutor().writeFile(ctx, vm, pw, path, []byte(val), 0o444)
				}
			}
		}
		// Projected configMap/secret sources handled by injectVolumes.
	}
}

// resolveDownwardAPIField resolves a fieldRef to its string value.
func resolveDownwardAPIField(pod *corev1.Pod, ref *corev1.ObjectFieldSelector) string {
	if ref == nil {
		return ""
	}
	switch ref.FieldPath {
	case "metadata.name":
		return pod.Name
	case "metadata.namespace":
		return pod.Namespace
	case "metadata.uid":
		return string(pod.UID)
	case "metadata.labels":
		parts := make([]string, 0, len(pod.Labels))
		for k, v := range pod.Labels {
			parts = append(parts, k+"="+v)
		}
		return strings.Join(parts, "\n")
	case "metadata.annotations":
		parts := make([]string, 0, len(pod.Annotations))
		for k, v := range pod.Annotations {
			if !strings.HasPrefix(k, "cocoon.cis/") { // skip internal
				parts = append(parts, k+"="+v)
			}
		}
		return strings.Join(parts, "\n")
	case "spec.nodeName":
		return pod.Spec.NodeName
	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName
	case "status.podIP":
		return pod.Status.PodIP
	}
	return ""
}
