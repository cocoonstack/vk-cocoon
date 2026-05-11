package cocoon

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/epoch/manifest"
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

	p.markLifecycleState(ctx, pod, meta.LifecycleStateCreating, "")

	// Adopt an existing local VM rather than creating a new one.
	if existing := p.vmByName(spec.VMName); existing != nil {
		p.applyRuntime(ctx, pod, existing)
		p.trackPod(pod, existing)
		p.startProbeIfEnabled(pod)
		p.refreshStatus(ctx, pod)
		p.notify(pod)
		p.markLifecycleState(ctx, pod, meta.LifecycleStateReady, "")
		metrics.PodLifecycleTotal.WithLabelValues("create", "adopted").Inc()
		return nil
	}

	bootStart := time.Now()
	v, sourceImage, err := p.bringUpVM(ctx, pod, spec)
	if err != nil {
		p.markLifecycleState(ctx, pod, meta.LifecycleStateFailed, err.Error())
		metrics.PodLifecycleTotal.WithLabelValues("create", "failed").Inc()
		return err
	}
	metrics.VMBootDuration.WithLabelValues(spec.Mode, spec.Backend).Observe(time.Since(bootStart).Seconds())

	// Resolve IP from cocoon-net lease before returning.
	if v.IP == "" && v.MAC != "" && p.LeaseParser != nil {
		if lease, err := p.LeaseParser.LookupByMAC(v.MAC); err == nil {
			v.IP = lease.IP
		}
	}

	p.applyRuntime(ctx, pod, v)
	// Capture isClonedBoot before goroutines mutate pod.Annotations.
	cloned := isClonedBoot(pod, spec)
	if spec.OS == string(cocoonv1.OSWindows) {
		p.goBackground(func() {
			p.applyWindowsStaticIP(p.lifecycleCtx, pod, v)
		})
	}
	if cloned {
		p.goBackground(func() {
			p.runPostCloneSetup(p.lifecycleCtx, pod, spec, v, sourceImage)
		})
	}
	p.trackPod(pod, v)
	// Start runs its first probe synchronously so refreshStatus below
	// already reflects the initial reachability.
	p.startProbeIfEnabled(pod)

	pod.Status.Phase = corev1.PodRunning
	now := metav1.Now()
	pod.Status.StartTime = &now
	p.refreshStatus(ctx, pod)
	p.notify(pod)
	if !cloned {
		// Cloned boots stay `creating` until runPostCloneSetup finishes.
		p.markLifecycleState(ctx, pod, meta.LifecycleStateReady, "")
	}
	metrics.PodLifecycleTotal.WithLabelValues("create", "ok").Inc()
	return nil
}

// bringUpVM dispatches on mode: unmanaged (adopt), clone, run, or fork.
// The returned sourceImage is the snapshot's original image (cloudimg URL
// or OCI ref) when available, used by post-clone classification.
func (p *Provider) bringUpVM(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec) (*vm.VM, string, error) {
	if !spec.Managed {
		runtime := meta.ParseVMRuntime(pod)
		if runtime.VMID == "" || runtime.IP == "" {
			return nil, "", fmt.Errorf("unmanaged vm %s missing pre-assigned IP/VMID", spec.VMName)
		}
		return &vm.VM{ID: runtime.VMID, Name: spec.VMName, IP: runtime.IP, State: vm.StateRunning}, "", nil
	}

	backend := spec.Backend
	noDirectIO := spec.NoDirectIO
	mode := strings.ToLower(spec.Mode)
	fromDir, err := parseCloneFromDirAnnotation(pod)
	if err != nil {
		return nil, "", err
	}
	switch {
	case fromDir != "":
		if mode == string(cocoonv1.AgentModeRun) {
			return nil, "", fmt.Errorf("annotation %s is incompatible with mode=run", meta.AnnotationCloneFromDir)
		}
		if spec.ForkFrom != "" {
			return nil, "", fmt.Errorf("annotation %s is incompatible with fork-from %q", meta.AnnotationCloneFromDir, spec.ForkFrom)
		}
		v, err := p.Runtime.Clone(ctx, vm.CloneOptions{
			FromDir:    fromDir,
			To:         spec.VMName,
			Network:    spec.Network,
			Backend:    backend,
			NoDirectIO: noDirectIO,
			OnDemand:   useOnDemandClone(spec),
		})
		if err != nil {
			metrics.CloneFromDirTotal.WithLabelValues("failed").Inc()
			return nil, "", fmt.Errorf("clone vm %s from dir %s: %w", spec.VMName, fromDir, err)
		}
		metrics.CloneFromDirTotal.WithLabelValues("ok").Inc()
		return v, "", nil

	case spec.ForkFrom != "":
		cloneFrom, err := p.ensureForkSnapshot(ctx, spec.ForkFrom)
		if err != nil {
			return nil, "", err
		}
		v, err := p.Runtime.Clone(ctx, vm.CloneOptions{
			From:       cloneFrom,
			To:         spec.VMName,
			Network:    spec.Network,
			Backend:    backend,
			NoDirectIO: noDirectIO,
			OnDemand:   useOnDemandClone(spec),
		})
		if err != nil {
			return nil, "", fmt.Errorf("clone vm %s from %s: %w", spec.VMName, cloneFrom, err)
		}
		return v, "", nil // forkFrom has no snapshot metadata

	case mode == string(cocoonv1.AgentModeRun):
		runImage, err := p.ensureRunImage(ctx, spec.Image, spec.ForcePull)
		if err != nil {
			return nil, "", fmt.Errorf("ensure image %s: %w", spec.Image, err)
		}
		cpu, memory := vmResourceOverrides(pod)
		v, err := p.Runtime.Run(ctx, vm.RunOptions{
			Image:      runImage,
			Name:       spec.VMName,
			CPU:        cpu,
			Memory:     memory,
			Network:    spec.Network,
			Storage:    spec.Storage,
			OS:         spec.OS,
			Force:      spec.ForcePull,
			Backend:    backend,
			NoDirectIO: noDirectIO,
		})
		if err != nil {
			return nil, "", fmt.Errorf("run vm %s: %w", spec.VMName, err)
		}
		// A fresh main VM must invalidate any fork snapshot cached from a
		// previous incarnation (e.g. VM kill + recreate, operator recreate),
		// so sub-agents that scale later clone from current state.
		forkName := forkSnapshotName(spec.VMName)
		if err := p.Runtime.SnapshotRemoveIfExists(ctx, forkName); err != nil {
			log.WithFunc("Provider.bringUpVM").Errorf(ctx, err, "invalidate fork snapshot %s", forkName)
		}
		return v, "", nil

	default: // clone is the default
		repo, tag := utils.ParseRef(spec.Image)
		local := localSnapshotName(repo, tag)
		snapshot, err := p.ensureSnapshot(ctx, repo, tag, local)
		if err != nil {
			metrics.SnapshotPullTotal.WithLabelValues("failed").Inc()
			return nil, "", fmt.Errorf("ensure snapshot %s: %w", local, err)
		}
		metrics.SnapshotPullTotal.WithLabelValues("ok").Inc()
		if backendErr := assertSnapshotBackend(snapshot, backend); backendErr != nil {
			return nil, "", fmt.Errorf("clone vm %s from %s: %w", spec.VMName, local, backendErr)
		}

		var srcImage string
		if snapshot != nil && snapshot.Image != "" {
			srcImage = snapshot.Image
		}

		v, err := p.Runtime.Clone(ctx, vm.CloneOptions{
			From:       local,
			To:         spec.VMName,
			Network:    spec.Network,
			Backend:    backend,
			NoDirectIO: noDirectIO,
			Pull:       srcImage != "",
			OnDemand:   useOnDemandClone(spec),
		})
		if err != nil {
			return nil, "", fmt.Errorf("clone vm %s from %s: %w", spec.VMName, local, err)
		}
		return v, srcImage, nil
	}
}

// parseCloneFromDirAnnotation returns the validated absolute, canonical
// path from the clone-from-dir annotation, or "" when absent.
func parseCloneFromDirAnnotation(pod *corev1.Pod) (string, error) {
	if pod == nil {
		return "", nil
	}
	raw := strings.TrimSpace(pod.Annotations[meta.AnnotationCloneFromDir])
	if raw == "" {
		return "", nil
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("annotation %s must be an absolute path, got %q", meta.AnnotationCloneFromDir, raw)
	}
	if cleaned := filepath.Clean(raw); cleaned != raw {
		return "", fmt.Errorf("annotation %s must be a canonical path, got %q (cleaned: %q)", meta.AnnotationCloneFromDir, raw, cleaned)
	}
	return raw, nil
}

// useOnDemandClone picks the cocoon `vm clone --on-demand` flag. Linux
// keeps lazy UFFD paging — clone return and first exec are both fast.
// Windows pays enough demand-page cost on the first PowerShell call
// that prefaulting the snapshot wins overall.
func useOnDemandClone(spec meta.VMSpec) bool {
	return spec.OS != string(cocoonv1.OSWindows)
}

// isClonedBoot reports whether bringUpVM took a clone path. CocoonSet
// sub-agents inherit mode=run from the parent spec but fork-clone from
// the main VM, so spec.Mode alone is insufficient — fromDir / ForkFrom
// must override.
func isClonedBoot(pod *corev1.Pod, spec meta.VMSpec) bool {
	if pod != nil && strings.TrimSpace(pod.Annotations[meta.AnnotationCloneFromDir]) != "" {
		return true
	}
	if spec.ForkFrom != "" {
		return true
	}
	return strings.ToLower(spec.Mode) != string(cocoonv1.AgentModeRun)
}

// assertSnapshotBackend rejects a clone when the target backend differs from
// the backend that produced the snapshot. CH and FC store state incompatibly,
// so letting this reach cocoon would fail with a harder-to-debug error.
func assertSnapshotBackend(snapshot *vm.Snapshot, targetBackend string) error {
	if snapshot == nil || snapshot.Hypervisor == "" || targetBackend == "" {
		return nil
	}
	if snapshot.Hypervisor == targetBackend {
		return nil
	}
	return fmt.Errorf("snapshot %s was taken with %s but CocoonSet requests %s",
		snapshot.Name, snapshot.Hypervisor, targetBackend)
}

// ensureRunImage materializes the base image locally and returns the ref
// `cocoon vm run` should be invoked with. Cloud-image artifacts pulled
// from epoch return the canonical /dl/{repo}/{tag} URL so vmCfg.Image
// (and any snapshot pushed back to epoch) stays portable across nodes;
// other kinds return the input unchanged.
func (p *Provider) ensureRunImage(ctx context.Context, image string, force bool) (string, error) {
	if image == "" {
		return image, nil
	}
	if p.Puller == nil || p.Puller.Registry == nil || isURLImage(image) {
		return image, p.Runtime.EnsureImage(ctx, image, force)
	}
	repo, tag := utils.ParseRef(image)
	raw, _, err := p.Puller.Registry.GetManifest(ctx, repo, tag)
	if err != nil {
		// Non-epoch ref or registry hiccup; cocoon image pull handles non-epoch refs natively.
		return image, p.Runtime.EnsureImage(ctx, image, force)
	}
	kind, classifyErr := manifest.Classify(raw)
	if classifyErr != nil {
		return image, p.Runtime.EnsureImage(ctx, image, force)
	}
	switch kind {
	case manifest.KindCloudImage:
		canonical := canonicalCloudImgURL(p.Puller.Registry.BaseURL(), repo, tag)
		return canonical, p.Puller.EnsureCloudImageFromRaw(ctx, repo, canonical, raw, force)
	case manifest.KindSnapshot:
		return image, fmt.Errorf("image %s is a snapshot artifact; use mode=clone instead of mode=run", image)
	default:
		return image, p.Runtime.EnsureImage(ctx, image, force)
	}
}

// canonicalCloudImgURL builds the /dl/{repo}/{tag} URL cocoon's cloudimg
// backend can pull via plain http.Get.
func canonicalCloudImgURL(baseURL, repo, tag string) string {
	return fmt.Sprintf("%s/dl/%s/%s", strings.TrimRight(baseURL, "/"), repo, tag)
}

// isURLImage reports whether ref looks like an HTTP(S) cloud-image URL.
func isURLImage(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
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
	pullStart := time.Now()
	if pullErr := p.Puller.PullSnapshot(ctx, repo, tag, local); pullErr != nil {
		return nil, pullErr
	}
	metrics.SnapshotPullDuration.Observe(time.Since(pullStart).Seconds())
	snapshot, err = p.Runtime.Snapshot(ctx, local)
	if err != nil {
		return nil, fmt.Errorf("inspect imported snapshot %s: %w", local, err)
	}
	return snapshot, nil
}

// ensureForkSnapshot returns the fork snapshot name for a source VM,
// creating it if missing and reusing it otherwise. Reuse matters because
// `snapshot save` pauses the source VM, dumps guest memory, and costs
// ~2s for a 1GiB Linux guest — paying that on every sub-agent creation
// multiplied the hot-scale path by 4–5×. Sub-agents of a CocoonSet are
// identical replicas, so the first checkpoint is the one that matters;
// to refresh the fork state, scale the set to zero and back up.
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
	rt := meta.VMRuntime{VMID: v.ID, IP: v.IP}
	rt.Apply(pod)
	p.patchRuntimeAnnotations(ctx, pod.Namespace, pod.Name, v)
}

func (p *Provider) patchRuntimeAnnotations(ctx context.Context, namespace, name string, v *vm.VM) {
	logger := log.WithFunc("Provider.patchRuntimeAnnotations")
	annos := map[string]any{
		meta.AnnotationVMID: v.ID,
		meta.AnnotationIP:   v.IP,
	}
	for range 3 {
		if err := p.patchPodAnnotations(ctx, namespace, name, annos); err == nil {
			return
		}
		if !commonk8s.SleepCtx(ctx, 500*time.Millisecond) {
			return
		}
	}
	logger.Warnf(ctx, "annotation patch failed after retries for %s/%s, will reconcile on restart", namespace, name)
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
