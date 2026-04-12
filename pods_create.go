package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/epoch/utils"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// CreatePod admits a pod by pulling its snapshot/image and creating the VM.
func (p *CocoonProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("CocoonProvider.CreatePod")
	logger.Infof(ctx, "create pod %s/%s", pod.Namespace, pod.Name)

	spec := meta.ParseVMSpec(pod)
	if spec.VMName == "" {
		metrics.PodLifecycleTotal.WithLabelValues("create", "missing_vmname").Inc()
		return fmt.Errorf("pod %s/%s missing %s annotation", pod.Namespace, pod.Name, meta.AnnotationVMName)
	}

	// Adopt an existing local VM rather than creating a new one.
	if existing := p.vmByName(spec.VMName); existing != nil {
		p.applyRuntime(pod, existing)
		p.trackPod(pod, existing)
		metrics.PodLifecycleTotal.WithLabelValues("create", "adopted").Inc()
		return nil
	}

	v, err := p.bringUpVM(ctx, pod, spec)
	if err != nil {
		metrics.PodLifecycleTotal.WithLabelValues("create", "failed").Inc()
		return err
	}

	// Resolve IP from dnsmasq lease before returning.
	if v.IP == "" && v.MAC != "" && p.LeaseParser != nil {
		if lease, err := p.LeaseParser.LookupByMAC(v.MAC); err == nil {
			v.IP = lease.IP
		}
	}

	p.applyRuntime(pod, v)
	p.trackPod(pod, v)
	if p.Probes != nil {
		// Start runs its first probe synchronously so refreshStatus below
		// already reflects the initial reachability.
		key := meta.PodKey(pod.Namespace, pod.Name)
		p.Probes.Start(key, p.buildProbe(pod.Namespace, pod.Name), p.buildOnUpdate(pod.Namespace, pod.Name))
	}

	pod.Status.Phase = corev1.PodRunning
	pod.Status.StartTime = nowPtr()
	p.refreshStatus(ctx, pod)
	p.notify(pod)
	metrics.PodLifecycleTotal.WithLabelValues("create", "ok").Inc()
	metrics.VMTableSize.Inc()
	return nil
}

// bringUpVM dispatches on mode: unmanaged (adopt), clone, run, or fork.
func (p *CocoonProvider) bringUpVM(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec) (*vm.VM, error) {
	cpu, memory := vmResourceOverrides(pod)
	mode := strings.ToLower(spec.Mode)
	switch {
	case !spec.Managed:
		runtime := meta.ParseVMRuntime(pod)
		if runtime.VMID == "" || runtime.IP == "" {
			return nil, fmt.Errorf("unmanaged vm %s missing pre-assigned IP/VMID", spec.VMName)
		}
		return &vm.VM{ID: runtime.VMID, Name: spec.VMName, IP: runtime.IP, State: vm.StateRunning}, nil

	case spec.ForkFrom != "":
		cloneFrom, err := p.ensureForkSnapshot(ctx, spec.ForkFrom)
		if err != nil {
			return nil, err
		}
		v, err := p.Runtime.Clone(ctx, vm.CloneOptions{
			From:    cloneFrom,
			To:      spec.VMName,
			CPU:     cpu,
			Memory:  memory,
			Network: spec.Network,
			Storage: spec.Storage,
		})
		if err != nil {
			return nil, fmt.Errorf("clone vm %s from %s: %w", spec.VMName, cloneFrom, err)
		}
		return v, nil

	case mode == string(cocoonv1.AgentModeRun):
		opts := vm.RunOptions{
			Image:   spec.Image,
			Name:    spec.VMName,
			CPU:     cpu,
			Memory:  memory,
			Network: spec.Network,
			Storage: spec.Storage,
			OS:      spec.OS,
		}
		v, err := p.Runtime.Run(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("run vm %s: %w", spec.VMName, err)
		}
		return v, nil

	default: // clone is the default
		from := spec.Image
		cloneFrom, _ := utils.ParseRef(from)
		snapshot, err := p.ensureSnapshot(ctx, from)
		if err != nil {
			metrics.SnapshotPullTotal.WithLabelValues("failed").Inc()
			return nil, fmt.Errorf("ensure snapshot %s: %w", from, err)
		}
		metrics.SnapshotPullTotal.WithLabelValues("ok").Inc()
		if snapshot != nil && snapshot.Image != "" {
			if ensureErr := p.Runtime.EnsureImage(ctx, snapshot.Image); ensureErr != nil {
				return nil, fmt.Errorf("ensure base image for snapshot %s: %w", cloneFrom, ensureErr)
			}
		}

		opts := vm.CloneOptions{
			From:    cloneFrom,
			To:      spec.VMName,
			CPU:     cpu,
			Memory:  memory,
			Network: spec.Network,
			Storage: spec.Storage,
		}
		v, err := p.Runtime.Clone(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("clone vm %s from %s: %w", spec.VMName, cloneFrom, err)
		}
		return v, nil
	}
}

// ensureSnapshot returns the local snapshot, pulling from epoch if needed.
// Lookup uses the bare repo name (tag stripped) because cocoon stores imports under that name.
func (p *CocoonProvider) ensureSnapshot(ctx context.Context, ref string) (*vm.Snapshot, error) {
	if ref == "" {
		return nil, nil
	}
	repo, tag := utils.ParseRef(ref)
	snapshot, err := p.Runtime.Snapshot(ctx, repo)
	if err == nil {
		return snapshot, nil
	}
	if p.Puller == nil {
		return nil, nil
	}
	if pullErr := p.Puller.PullSnapshot(ctx, repo, tag, ""); pullErr != nil {
		return nil, pullErr
	}
	snapshot, err = p.Runtime.Snapshot(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("inspect imported snapshot %s: %w", repo, err)
	}
	return snapshot, nil
}

// ensureForkSnapshot creates a cloneable local snapshot for sub-agents,
// since cocoon's clone requires a snapshot ref, not a live VM name.
func (p *CocoonProvider) ensureForkSnapshot(ctx context.Context, sourceVMName string) (string, error) {
	snapshotName := forkSnapshotName(sourceVMName)
	if _, err := p.Runtime.Snapshot(ctx, snapshotName); err == nil {
		return snapshotName, nil
	}

	sourceVM := p.vmByName(sourceVMName)
	if sourceVM == nil {
		inspected, err := p.Runtime.Inspect(ctx, sourceVMName)
		if err != nil {
			return "", fmt.Errorf("inspect fork source vm %s: %w", sourceVMName, err)
		}
		sourceVM = inspected
	}
	if err := p.Runtime.SnapshotSave(ctx, snapshotName, sourceVM.ID); err != nil {
		return "", fmt.Errorf("snapshot fork source vm %s as %s: %w", sourceVMName, snapshotName, err)
	}
	return snapshotName, nil
}

func forkSnapshotName(sourceVMName string) string {
	return "fork-" + sourceVMName
}

// vmByName looks up a VM by name.
func (p *CocoonProvider) vmByName(name string) *vm.VM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.vmsByName[name]
}

// applyRuntime writes VMID/IP annotations onto the pod.
func (p *CocoonProvider) applyRuntime(pod *corev1.Pod, v *vm.VM) {
	runtime := meta.VMRuntime{VMID: v.ID, IP: v.IP}
	runtime.Apply(pod)
}

// vmResourceOverrides translates pod resources into cocoon CLI args (milliCPU rounds up).
func vmResourceOverrides(pod *corev1.Pod) (int, string) {
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return 0, ""
	}
	resources := pod.Spec.Containers[0].Resources
	cpu := selectQuantity(resources.Requests, resources.Limits, corev1.ResourceCPU)
	memory := selectQuantity(resources.Requests, resources.Limits, corev1.ResourceMemory)
	return quantityCPURoundUp(cpu), quantityBytes(memory)
}

func selectQuantity(requests, limits corev1.ResourceList, name corev1.ResourceName) resource.Quantity {
	if q, ok := requests[name]; ok && !q.IsZero() {
		return q
	}
	if q, ok := limits[name]; ok && !q.IsZero() {
		return q
	}
	return resource.Quantity{}
}

func quantityCPURoundUp(q resource.Quantity) int {
	if q.IsZero() {
		return 0
	}
	milli := q.MilliValue()
	if milli <= 0 {
		return 0
	}
	return int((milli + 999) / 1000)
}

func quantityBytes(q resource.Quantity) string {
	if q.IsZero() {
		return ""
	}
	if bytes := q.Value(); bytes > 0 {
		return strconv.FormatInt(bytes, 10)
	}
	return ""
}

func (p *CocoonProvider) refreshStatus(ctx context.Context, pod *corev1.Pod) {
	if pod == nil {
		return
	}
	status, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name)
	if err != nil || status == nil {
		return
	}
	pod.Status = *status
}

func nowPtr() *metav1.Time {
	t := metav1.NewTime(time.Now().UTC())
	return &t
}
