package provider

import (
	"context"
	"os/exec"
	"path/filepath"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
)

type deleteRequest struct {
	key string
	pod *corev1.Pod
	vm  *CocoonVM
	ok  bool
}

func (p *CocoonProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	req := p.loadDeleteRequest(pod)
	logger := log.WithFunc("provider.DeletePod")
	logger.Infof(ctx, "%s", req.key)

	switch {
	case req.ok && req.vm.vmID != "" && req.vm.managed:
		p.deleteManagedVM(ctx, req)
	case req.ok && !req.vm.managed:
		logger.Infof(ctx, "%s: skipping unmanaged VM %s (%s)", req.key, req.vm.vmName, req.vm.vmID)
	case !req.ok:
		p.deleteFallbackVM(ctx, req)
	}

	p.cleanupDeletedPod(req.key, pod.Name)
	return nil
}

func (p *CocoonProvider) loadDeleteRequest(pod *corev1.Pod) deleteRequest {
	key := podKey(pod.Namespace, pod.Name)

	p.mu.RLock()
	vm, ok := p.vms[key]
	p.mu.RUnlock()

	return deleteRequest{
		key: key,
		pod: pod,
		vm:  vm,
		ok:  ok,
	}
}

func (p *CocoonProvider) deleteManagedVM(ctx context.Context, req deleteRequest) {
	logger := log.WithFunc("provider.DeletePod")
	if ownerKey := p.findOtherActivePodForVMID(ctx, req.pod, req.vm.vmID); ownerKey != "" {
		logger.Warnf(ctx, "%s: skip destroy for VM %s (%s); still owned by active pod %s", req.key, req.vm.vmName, req.vm.vmID, ownerKey)
		return
	}

	if p.shouldSnapshotOnDelete(ctx, req.pod) {
		p.snapshotBeforeDelete(ctx, req)
	} else {
		p.clearScaleDownSnapshot(ctx, req)
	}

	p.destroyVM(ctx, req.key, req.vm.vmName, req.vm.vmID)
}

func (p *CocoonProvider) snapshotBeforeDelete(ctx context.Context, req deleteRequest) {
	logger := log.WithFunc("provider.DeletePod")
	p.mu.Lock()
	req.vm.state = stateSuspending
	p.mu.Unlock()

	spec := resolvePodSpec(req.pod)
	puller := p.getPuller(ctx, spec.registryURL)
	snapshots := p.snapshotManager()
	snapshotName := req.vm.vmName + "-suspend"

	logger.Infof(ctx, "%s: creating snapshot %s from running VM %s", req.key, snapshotName, req.vm.vmID)
	out, err := snapshots.saveSnapshot(ctx, snapshotName, req.vm.vmID)
	if err != nil {
		logger.Errorf(ctx, err, "%s: snapshot failed: %s", req.key, out)
		return
	}

	logger.Infof(ctx, "%s: snapshot %s created", req.key, snapshotName)
	pushedToEpoch := p.pushDeleteSnapshot(ctx, req, puller, snapshotName)
	fullRef := snapshotName
	if spec.registryURL != "" && pushedToEpoch {
		fullRef = spec.registryURL + "/" + snapshotName
	}
	snapshots.recordSuspendedSnapshot(ctx, req.pod, req.vm.vmName, fullRef)
	if pushedToEpoch {
		snapshots.removeSnapshot(ctx, snapshotName)
	}
}

func (p *CocoonProvider) pushDeleteSnapshot(ctx context.Context, req deleteRequest, puller *EpochPuller, snapshotName string) bool {
	if puller == nil {
		return false
	}

	logger := log.WithFunc("provider.DeletePod")
	_ = exec.CommandContext(ctx, "sudo", "chmod", "-R", "a+rX", //nolint:gosec // trusted path from config
		filepath.Join(puller.RootDir(), "snapshot", "localfile")).Run()

	if err := puller.PushSnapshot(ctx, snapshotName, "latest"); err != nil {
		logger.Errorf(ctx, err, "%s: epoch push failed", req.key)
		return false
	}

	logger.Infof(ctx, "%s: snapshot pushed to epoch", req.key)
	return true
}

func (p *CocoonProvider) clearScaleDownSnapshot(ctx context.Context, req deleteRequest) {
	logger := log.WithFunc("provider.DeletePod")
	logger.Infof(ctx, "%s: scale-down detected, skipping snapshot", req.key)
	if isMainAgent(req.vm.vmName) {
		p.snapshotManager().clearSuspendedSnapshot(ctx, req.pod.Namespace, req.vm.vmName)
	}
}

func (p *CocoonProvider) deleteFallbackVM(ctx context.Context, req deleteRequest) {
	vmID := ann(req.pod, AnnVMID, "")
	managed := ann(req.pod, AnnManaged, "")
	if vmID == "" || managed != valTrue {
		return
	}

	logger := log.WithFunc("provider.DeletePod")
	if ownerKey := p.findOtherActivePodForVMID(ctx, req.pod, vmID); ownerKey != "" {
		logger.Warnf(ctx, "%s: skip fallback destroy for vm-id=%s; still owned by active pod %s", req.key, vmID, ownerKey)
		return
	}

	logger.Infof(ctx, "%s: fallback destroy via annotation vm-id=%s", req.key, vmID)
	_, _ = p.cocoonExec(ctx, "vm", "rm", "--force", vmID)
}

func (p *CocoonProvider) destroyVM(ctx context.Context, key, vmName, vmID string) {
	log.WithFunc("provider.DeletePod").Infof(ctx, "%s: destroying VM %s (%s)", key, vmName, vmID)
	_, _ = p.cocoonExec(ctx, "vm", "rm", "--force", vmID)
}

func (p *CocoonProvider) cleanupDeletedPod(key, podName string) {
	p.stopProbes(key)
	removePodDNS(podName)
	p.mu.Lock()
	delete(p.pods, key)
	delete(p.vms, key)
	delete(p.injectHashes, key+"/env")
	delete(p.injectHashes, key+"/vol")
	p.mu.Unlock()
}
