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
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// CreatePod is the virtual-kubelet entry point for pod admission.
// It reads the meta.VMSpec contract written by the operator (or
// webhook), pulls the snapshot or cloud image from epoch if needed,
// asks the cocoon runtime to clone or boot the VM, then writes the
// runtime metadata back into the pod's annotations and stores it
// in the in-memory table.
func (p *CocoonProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("CocoonProvider.CreatePod")
	logger.Infof(ctx, "create pod %s/%s", pod.Namespace, pod.Name)

	spec := meta.ParseVMSpec(pod)
	if spec.VMName == "" {
		metrics.PodLifecycleTotal.WithLabelValues("create", "missing_vmname").Inc()
		return fmt.Errorf("pod %s/%s missing %s annotation", pod.Namespace, pod.Name, meta.AnnotationVMName)
	}

	// If a VM with that name already exists locally, adopt it
	// rather than creating a new one. This is the path startup
	// reconcile takes for pods we already own.
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

	// Resolve the IP from the local dnsmasq lease file before
	// returning so subsequent GetPodStatus calls have something
	// real to report.
	if v.IP == "" && v.MAC != "" && p.LeaseParser != nil {
		if lease, err := p.LeaseParser.LookupByMAC(v.MAC); err == nil {
			v.IP = lease.IP
		}
	}

	p.applyRuntime(pod, v)
	p.trackPod(pod, v)
	if p.Probes != nil {
		p.Probes.MarkReady(podKey(pod.Namespace, pod.Name))
	}

	pod.Status.Phase = corev1.PodRunning
	pod.Status.StartTime = nowPtr()
	p.refreshStatus(ctx, pod)
	p.notify(pod)
	metrics.PodLifecycleTotal.WithLabelValues("create", "ok").Inc()
	metrics.VMTableSize.Inc()
	return nil
}

// bringUpVM does the actual cocoon CLI work for a fresh pod. The
// strategy is mode-driven, dispatched on the typed enum constants
// from cocoon-common rather than bare strings:
//
//   - clone: pull the snapshot from epoch (if not already local)
//     and ask cocoon to clone it.
//   - run:   ask cocoon to boot a fresh VM from the supplied image.
//   - static (toolboxes only): skip the runtime, adopt the
//     pre-assigned IP / VMID the operator already wrote into the
//     VMRuntime annotations on the pod.
func (p *CocoonProvider) bringUpVM(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec) (*vm.VM, error) {
	cpu, memory := vmResourceOverrides(pod)
	mode := strings.ToLower(spec.Mode)
	switch {
	case mode == string(cocoonv1.ToolboxModeStatic):
		runtime := meta.ParseVMRuntime(pod)
		if runtime.VMID == "" || runtime.IP == "" {
			return nil, fmt.Errorf("static toolbox %s missing static IP/VMID", spec.VMName)
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
		// Top-level pods clone from the snapshot referenced by Image;
		// the ForkFrom branch above handles the sub-agent fork path.
		from := spec.Image
		cloneFrom, _ := splitRef(from)
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

// ensureSnapshot returns the local snapshot metadata, pulling it
// from epoch when it is not already cached locally. The lookup uses
// the bare repo name (tag stripped) because cocoon stores imported
// snapshots under that name — an earlier version passed the full
// ref including the tag and never matched.
func (p *CocoonProvider) ensureSnapshot(ctx context.Context, ref string) (*vm.Snapshot, error) {
	if ref == "" {
		return nil, nil
	}
	repo, tag := splitRef(ref)
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

// ensureForkSnapshot makes a cloneable local snapshot for sub-agents.
// cocoon's `vm clone` CLI restores from snapshot refs, not live VM
// names, so a local fork must materialize a snapshot first.
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

// vmByName looks up a VM in the in-memory table by name.
func (p *CocoonProvider) vmByName(name string) *vm.VM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.vmsByName[name]
}

// applyRuntime writes the runtime VMRuntime annotations onto a pod
// so the operator can observe the VMID/IP vk-cocoon just learned.
func (p *CocoonProvider) applyRuntime(pod *corev1.Pod, v *vm.VM) {
	runtime := meta.VMRuntime{VMID: v.ID, IP: v.IP}
	runtime.Apply(pod)
}

// splitRef splits an OCI ref of the form "name:tag" into its
// components. Refs without a tag default to "latest".
func splitRef(ref string) (string, string) {
	idx := strings.LastIndex(ref, ":")
	if idx < 0 {
		return ref, "latest"
	}
	return ref[:idx], ref[idx+1:]
}

// vmResourceOverrides translates pod resource requirements into the cocoon CLI
// resource model. cocoon only accepts whole CPUs, so milliCPU values round up.
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
