package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const suspendedSnapshotConfigMap = "cocoon-vm-snapshots"

type snapshotManager struct {
	provider *CocoonProvider
}

func (p *CocoonProvider) snapshotManager() *snapshotManager {
	return &snapshotManager{provider: p}
}

func (s *snapshotManager) saveSnapshot(ctx context.Context, snapshotName, vmID string) (string, error) {
	if s == nil || s.provider == nil {
		return "", fmt.Errorf("snapshot manager is not configured")
	}
	s.removeSnapshot(ctx, snapshotName)
	return s.provider.cocoonExec(ctx, "snapshot", "save", "--name", snapshotName, vmID)
}

func (s *snapshotManager) removeSnapshot(ctx context.Context, snapshotName string) {
	if s == nil || s.provider == nil {
		return
	}
	_, _ = s.provider.cocoonExec(ctx, "snapshot", "rm", snapshotName)
}

func (s *snapshotManager) recordSuspendedSnapshot(ctx context.Context, pod *corev1.Pod, vmName, snapshotRef string) {
	if s == nil || s.provider == nil || s.provider.kubeClient == nil || vmName == "" || snapshotRef == "" {
		return
	}
	logger := log.WithFunc("provider.recordSuspendedSnapshot")
	ns := pod.Namespace
	cmClient := s.provider.kubeClient.CoreV1().ConfigMaps(ns)

	_, err := cmClient.Get(ctx, suspendedSnapshotConfigMap, metav1.GetOptions{})
	if err != nil {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: suspendedSnapshotConfigMap, Namespace: ns},
			Data:       map[string]string{},
		}
		if _, createErr := cmClient.Create(ctx, cm, metav1.CreateOptions{}); createErr != nil {
			logger.Warnf(ctx, "%s: create configmap: %v", vmName, createErr)
			return
		}
	}

	patch, _ := json.Marshal(map[string]any{
		"data": map[string]string{vmName: snapshotRef},
	})
	if _, err := cmClient.Patch(ctx, suspendedSnapshotConfigMap, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		logger.Warnf(ctx, "%s: %v", vmName, err)
	} else {
		logger.Infof(ctx, "%s -> %s", vmName, snapshotRef)
	}
}

func (s *snapshotManager) lookupSuspendedSnapshot(ctx context.Context, ns, vmName string) string {
	if s == nil || s.provider == nil {
		return ""
	}
	if s.provider.lookupSuspendedSnapshotFn != nil {
		return s.provider.lookupSuspendedSnapshotFn(ctx, ns, vmName)
	}
	if s.provider.kubeClient == nil {
		return ""
	}
	cm, err := s.provider.kubeClient.CoreV1().ConfigMaps(ns).Get(ctx, suspendedSnapshotConfigMap, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return cm.Data[vmName]
}

func (s *snapshotManager) clearSuspendedSnapshot(ctx context.Context, ns, vmName string) {
	if s == nil || s.provider == nil || s.provider.kubeClient == nil {
		return
	}
	patch := fmt.Sprintf(`[{"op":"remove","path":"/data/%s"}]`, vmName)
	_, _ = s.provider.kubeClient.CoreV1().ConfigMaps(ns).Patch(ctx, suspendedSnapshotConfigMap, types.JSONPatchType, []byte(patch), metav1.PatchOptions{})
}
