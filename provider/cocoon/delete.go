package cocoon

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func (p *Provider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("Provider.DeletePod")
	logger.Infof(ctx, "delete pod %s/%s", pod.Namespace, pod.Name)

	key := meta.PodKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	p.deleting[key] = struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.deleting, key)
		p.mu.Unlock()
	}()

	if err := p.backoffIfResuming(pod.Namespace, pod.Name); err != nil {
		return err
	}
	spec := meta.ParseVMSpec(pod)
	if isMacosSpec(spec) {
		return p.deleteMacosPod(ctx, pod, spec)
	}
	// a seat release keeps the local snapshot as the same-node warm-wake cache; resolveWakeSource still gates it on the :hibernate tag.
	keepSnapshots := meta.ReadKeepSnapshotOnDelete(pod)

	v := p.vmForPod(pod.Namespace, pod.Name)
	if v == nil {
		reason := "no_vm"
		if keepSnapshots {
			reason = "seat_release"
		} else {
			p.removeLocalSnapshots(ctx, spec.VMName)
		}
		p.forgetPod(pod.Namespace, pod.Name)
		metrics.PodLifecycleTotal.WithLabelValues("delete", "skipped", reason).Inc()
		return nil
	}

	if meta.ShouldSnapshotVM(spec, meta.RoleForPod(pod, spec.VMName)) && p.Pusher != nil && v.Name != "" {
		p.saveAndPushSnapshot(ctx, v.Name, v.ID, meta.DefaultSnapshotTag, spec.Image)
	}

	if err := p.removeVM(ctx, v); err != nil {
		metrics.PodLifecycleTotal.WithLabelValues("delete", "failed", "").Inc()
		return fmt.Errorf("remove vm %s: %w", v.ID, err)
	}

	if !keepSnapshots {
		p.removeLocalSnapshots(ctx, v.Name)
	}

	p.forgetPod(pod.Namespace, pod.Name)
	pod.Status.Phase = corev1.PodSucceeded
	p.notify(pod)
	metrics.PodLifecycleTotal.WithLabelValues("delete", "ok", "").Inc()
	return nil
}

func (p *Provider) removeVM(ctx context.Context, v *vm.VM) error {
	if err := p.Runtime.Remove(ctx, v.ID); err != nil {
		return err
	}
	p.releaseDHCPLeases(ctx, v)
	return nil
}

func (p *Provider) releaseDHCPLeases(ctx context.Context, v *vm.VM) {
	if v == nil || p.LeaseReleaser == nil {
		return
	}
	logger := log.WithFunc("Provider.releaseDHCPLeases")
	releaseCtx := context.WithoutCancel(ctx)
	for _, mac := range dhcpMACs(v) {
		if err := p.LeaseReleaser.ReleaseByMAC(releaseCtx, mac); err != nil {
			// the VM is already gone; keep deletion progressing and let the lease expiry fallback handle a down cocoon-net daemon.
			logger.Warnf(ctx, "release DHCP lease for VM %s MAC %s: %v", v.ID, mac, err)
			metrics.LeaseReleaseTotal.WithLabelValues("failed").Inc()
			continue
		}
		metrics.LeaseReleaseTotal.WithLabelValues("ok").Inc()
	}
}

func (p *Provider) removeLocalSnapshots(ctx context.Context, vmName string) {
	if vmName == "" {
		return
	}
	// synchronous on purpose: an immediate recreate must not race the rm.
	var wg sync.WaitGroup
	for _, name := range []string{vmName, forkSnapshotName(vmName)} {
		wg.Go(func() {
			p.removeSnapshotDetached(ctx, "Provider.DeletePod", name)
		})
	}
	wg.Wait()
}

func dhcpMACs(v *vm.VM) []string {
	if len(v.NetworkConfigs) == 0 {
		if mac := strings.TrimSpace(v.MAC); mac != "" {
			return []string{mac}
		}
		return nil
	}

	macs := make([]string, 0, len(v.NetworkConfigs))
	for _, nic := range v.NetworkConfigs {
		mac := strings.TrimSpace(nic.MAC)
		if mac == "" || isStaticNIC(nic) {
			continue
		}
		macs = append(macs, mac)
	}
	return macs
}
