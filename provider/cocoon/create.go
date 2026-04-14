package cocoon

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
	"k8s.io/apimachinery/pkg/types"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/epoch/utils"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// CreatePod admits a pod by pulling its snapshot/image and creating the VM.
func (p *Provider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("Provider.CreatePod")
	logger.Infof(ctx, "create pod %s/%s", pod.Namespace, pod.Name)

	spec := meta.ParseVMSpec(pod)
	if spec.VMName == "" {
		metrics.PodLifecycleTotal.WithLabelValues("create", "missing_vmname").Inc()
		return fmt.Errorf("pod %s/%s missing %s annotation", pod.Namespace, pod.Name, meta.AnnotationVMName)
	}

	// Adopt an existing local VM rather than creating a new one.
	if existing := p.vmByName(spec.VMName); existing != nil {
		p.applyRuntime(ctx, pod, existing)
		p.trackPod(pod, existing)
		p.startProbeIfEnabled(pod)
		p.refreshStatus(ctx, pod)
		p.notify(pod)
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

	p.applyRuntime(ctx, pod, v)
	p.trackPod(pod, v)
	// Start runs its first probe synchronously so refreshStatus below
	// already reflects the initial reachability.
	p.startProbeIfEnabled(pod)

	pod.Status.Phase = corev1.PodRunning
	pod.Status.StartTime = nowPtr()
	p.refreshStatus(ctx, pod)
	p.notify(pod)
	metrics.PodLifecycleTotal.WithLabelValues("create", "ok").Inc()
	metrics.VMTableSize.Inc()
	return nil
}

// bringUpVM dispatches on mode: unmanaged (adopt), clone, run, or fork.
func (p *Provider) bringUpVM(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec) (*vm.VM, error) {
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
		repo, tag := utils.ParseRef(spec.Image)
		local := localSnapshotName(repo, tag)
		snapshot, err := p.ensureSnapshot(ctx, repo, tag, local)
		if err != nil {
			metrics.SnapshotPullTotal.WithLabelValues("failed").Inc()
			return nil, fmt.Errorf("ensure snapshot %s: %w", local, err)
		}
		metrics.SnapshotPullTotal.WithLabelValues("ok").Inc()
		if snapshot != nil && snapshot.Image != "" {
			if ensureErr := p.Runtime.EnsureImage(ctx, snapshot.Image); ensureErr != nil {
				return nil, fmt.Errorf("ensure base image for snapshot %s: %w", local, ensureErr)
			}
		}

		opts := vm.CloneOptions{
			From:    local,
			To:      spec.VMName,
			CPU:     cpu,
			Memory:  memory,
			Network: spec.Network,
			Storage: spec.Storage,
		}
		v, err := p.Runtime.Clone(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("clone vm %s from %s: %w", spec.VMName, local, err)
		}
		return v, nil
	}
}

// ensureSnapshot returns the local snapshot, pulling from epoch if needed.
// The local snapshot name includes the tag so that different tags of the
// same repo are stored separately (e.g. "myvm:v1" and "myvm:v2").
func (p *Provider) ensureSnapshot(ctx context.Context, repo, tag, local string) (*vm.Snapshot, error) {
	if repo == "" {
		return nil, nil
	}
	snapshot, err := p.Runtime.Snapshot(ctx, local)
	if err == nil {
		return snapshot, nil
	}
	if p.Puller == nil {
		return nil, nil
	}
	if pullErr := p.Puller.PullSnapshot(ctx, repo, tag, local); pullErr != nil {
		return nil, pullErr
	}
	snapshot, err = p.Runtime.Snapshot(ctx, local)
	if err != nil {
		return nil, fmt.Errorf("inspect imported snapshot %s: %w", local, err)
	}
	return snapshot, nil
}

// ensureForkSnapshot creates a cloneable local snapshot for sub-agents,
// since cocoon's clone requires a snapshot ref, not a live VM name.
func (p *Provider) ensureForkSnapshot(ctx context.Context, sourceVMName string) (string, error) {
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

// vmByName looks up a VM by name.
func (p *Provider) vmByName(name string) *vm.VM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.vmsByName[name]
}

// applyRuntime writes VMID/IP annotations onto the in-memory pod and
// patches them back to the API server so they survive provider restarts.
func (p *Provider) applyRuntime(ctx context.Context, pod *corev1.Pod, v *vm.VM) {
	runtime := meta.VMRuntime{VMID: v.ID, IP: v.IP}
	runtime.Apply(pod)
	p.patchRuntimeAnnotations(ctx, pod.Namespace, pod.Name, v)
}

// patchRuntimeAnnotations patches VMID/IP annotations back to the API server.
func (p *Provider) patchRuntimeAnnotations(ctx context.Context, namespace, name string, v *vm.VM) {
	if p.Clientset == nil {
		return
	}
	logger := log.WithFunc("Provider.patchRuntimeAnnotations")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	patch, err := commonk8s.AnnotationsMergePatch(map[string]any{
		meta.AnnotationVMID: v.ID,
		meta.AnnotationIP:   v.IP,
	})
	if err != nil {
		logger.Warnf(ctx, "marshal annotations %s/%s: %v", namespace, name, err)
		return
	}
	if _, err := p.Clientset.CoreV1().Pods(namespace).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		logger.Warnf(ctx, "patch annotations %s/%s: %v", namespace, name, err)
	}
}

func (p *Provider) startProbeIfEnabled(pod *corev1.Pod) {
	if p.Probes == nil {
		return
	}
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.Probes.Start(key, p.buildProbe(pod.Namespace, pod.Name), p.buildOnUpdate(pod.Namespace, pod.Name))
}

func (p *Provider) refreshStatus(ctx context.Context, pod *corev1.Pod) {
	if pod == nil {
		return
	}
	status, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name)
	if err != nil || status == nil {
		return
	}
	pod.Status = *status
}

// localSnapshotName builds the cocoon-local snapshot name from a repo and tag.
// The default tag is omitted for backward compatibility with existing snapshots.
func localSnapshotName(repo, tag string) string {
	if tag == "" || tag == meta.DefaultSnapshotTag {
		return repo
	}
	return repo + ":" + tag
}

func forkSnapshotName(sourceVMName string) string {
	return "fork-" + sourceVMName
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

func nowPtr() *metav1.Time {
	t := metav1.NewTime(time.Now().UTC())
	return &t
}
