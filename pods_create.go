package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	logger := providerLogger("CreatePod")
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
	p.notify(pod)
	metrics.PodLifecycleTotal.WithLabelValues("create", "ok").Inc()
	metrics.VMTableSize.Inc()
	return nil
}

// bringUpVM does the actual cocoon CLI work for a fresh pod. The
// strategy is mode-driven:
//
//   - clone: pull the snapshot from epoch (if not already local)
//     and ask cocoon to clone it.
//   - run:   ask cocoon to boot a fresh VM from the supplied image.
//
// Static toolboxes (mode=static) skip both paths and only adopt the
// pre-assigned IP / VMID; the operator already wrote those into the
// VMRuntime annotations on the pod.
func (p *CocoonProvider) bringUpVM(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec) (*vm.VM, error) {
	switch strings.ToLower(spec.Mode) {
	case "static":
		runtime := meta.ParseVMRuntime(pod)
		if runtime.VMID == "" || runtime.IP == "" {
			return nil, fmt.Errorf("static toolbox %s missing static IP/VMID", spec.VMName)
		}
		return &vm.VM{ID: runtime.VMID, Name: spec.VMName, IP: runtime.IP, State: "running"}, nil

	case "run":
		opts := vm.RunOptions{
			Image:    spec.Image,
			Name:     spec.VMName,
			Network:  spec.Network,
			Storage:  spec.Storage,
			OS:       spec.OS,
			NodeName: p.NodeName,
		}
		return p.Runtime.Run(ctx, opts)

	default: // clone is the default
		if err := p.ensureSnapshot(ctx, spec); err != nil {
			metrics.SnapshotPullTotal.WithLabelValues("failed").Inc()
			return nil, fmt.Errorf("ensure snapshot %s: %w", spec.Image, err)
		}
		metrics.SnapshotPullTotal.WithLabelValues("ok").Inc()

		from := spec.Image
		if spec.ForkFrom != "" {
			from = spec.ForkFrom
		}
		opts := vm.CloneOptions{
			From:     from,
			To:       spec.VMName,
			Network:  spec.Network,
			Storage:  spec.Storage,
			NodeName: p.NodeName,
		}
		return p.Runtime.Clone(ctx, opts)
	}
}

// ensureSnapshot pulls the snapshot from epoch when it is not
// already cached locally. The cocoon CLI is the source of truth
// for "is it cached"; we ask the runtime to inspect the name and
// only pull when it is missing.
func (p *CocoonProvider) ensureSnapshot(ctx context.Context, spec meta.VMSpec) error {
	if p.Puller == nil || spec.Image == "" {
		return nil
	}
	// Quick local check: if a VM with the same name as the source
	// already exists locally we treat the snapshot as cached.
	if local, err := p.Runtime.Inspect(ctx, spec.Image); err == nil && local != nil {
		return nil
	}
	repo, tag := splitRef(spec.Image)
	return p.Puller.PullSnapshot(ctx, repo, tag, "")
}

// vmByName looks up a VM in the in-memory table by name. Used by
// CreatePod's adopt path.
func (p *CocoonProvider) vmByName(name string) *vm.VM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.vmsByName[name]
}

// applyRuntime writes the runtime VMRuntime annotations on the pod
// so the operator can pick up the IP / VMID it just learned about.
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

func nowPtr() *metav1.Time {
	t := metav1.NewTime(time.Now().UTC())
	return &t
}
