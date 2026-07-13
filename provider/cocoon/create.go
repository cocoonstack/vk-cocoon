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
	"github.com/cocoonstack/cocoon-common/manifest"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/ociutil"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// CreatePod admits a pod by pulling its snapshot/image and creating the VM.
func (p *Provider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("Provider.CreatePod")
	logger.Infof(ctx, "create pod %s/%s", pod.Namespace, pod.Name)

	spec := meta.ParseVMSpec(pod)
	if spec.VMName == "" {
		metrics.PodLifecycleTotal.WithLabelValues("create", "skipped", "missing_vmname").Inc()
		return fmt.Errorf("pod %s/%s missing %s annotation", pod.Namespace, pod.Name, meta.AnnotationVMName)
	}

	p.markLifecycleState(ctx, pod, meta.LifecycleStateCreating, "")

	if existing := p.vmByName(spec.VMName); existing != nil {
		p.applyRuntime(ctx, pod, existing)
		p.trackPod(pod, existing)
		p.startProbeIfEnabled(pod)
		p.refreshStatus(ctx, pod)
		p.notify(pod)
		p.markLifecycleState(ctx, pod, meta.LifecycleStateReady, "")
		metrics.PodLifecycleTotal.WithLabelValues("create", "ok", "adopted").Inc()
		return nil
	}

	// A restore reuses wake()'s post-restore path (CH+Windows waits on the fresh
	// NIC's lease; others run runPostCloneSetup) and skips the base-image post-clone.
	restoring := meta.ReadRestoreFromHibernate(pod)
	bootStart := time.Now()
	v, sourceImage, err := p.bringUpVM(ctx, pod, spec)
	if err != nil {
		// A restore is a wake: count its failure like wake() does, not just create.
		if restoring {
			metrics.WakeTotal.WithLabelValues("failed").Inc()
		}
		p.failOp(ctx, pod, "CreateBringUpFailed", "create", err)
		return err
	}
	// A restore is a clone-from-hibernate; label its boot "clone" like wake does.
	bootMode := spec.Mode
	if restoring {
		bootMode = "clone"
	}
	metrics.VMBootDuration.WithLabelValues(bootMode, spec.Backend).Observe(time.Since(bootStart).Seconds())

	if v.IP == "" && v.MAC != "" && p.LeaseParser != nil {
		if lease, err := p.LeaseParser.LookupByMAC(v.MAC); err == nil {
			v.IP = lease.IP
		}
	}

	p.applyRuntime(ctx, pod, v)
	// Capture isClonedBoot before goroutines mutate pod.Annotations.
	cloned := isClonedBoot(pod, spec)
	// trackPod first: goroutines below call markLifecycleState, which reads p.pods[key].
	p.trackPod(pod, v)
	willRunSAC := p.willRunSAC(spec, v)
	if restoring {
		p.dispatchHibernateRestore(pod, spec, v, "create")
	}
	if spec.OS == string(cocoonv1.OSWindows) && !restoring {
		p.goBackground(func() {
			ran, err := p.applyWindowsStaticIP(p.lifecycleCtx, pod, v)
			if err != nil {
				metrics.PostCloneTotal.WithLabelValues("sac", "failed").Inc()
				p.markPostCloneState(p.lifecycleCtx, pod, postCloneStateFailed)
				errMsg := err.Error()
				p.emitWarningf(pod, "WindowsStaticIPFailed", "%s", truncate("create: "+errMsg, eventMessageMaxBytes))
				p.markLifecycleState(p.lifecycleCtx, pod, meta.LifecycleStateFailed, truncate(errMsg, lifecycleMessageMaxBytes))
				log.WithFunc("Provider.CreatePod").Errorf(p.lifecycleCtx, err,
					"%s/%s windows static IP", pod.Namespace, pod.Name)
				return
			}
			if ran {
				metrics.PostCloneTotal.WithLabelValues("sac", "ok").Inc()
				// Non-clone Ready was deferred to here so watchers don't see a transient Ready.
				if !cloned && !p.lifecycleAlreadyFailed(pod) {
					p.markLifecycleState(p.lifecycleCtx, pod, meta.LifecycleStateReady, "")
				}
			}
		})
	}
	if cloned && !restoring {
		p.goBackground(func() {
			p.runPostCloneSetup(p.lifecycleCtx, pod, spec, v, sourceImage, "create")
		})
	}
	// First probe is synchronous so refreshStatus below sees its result.
	p.startProbeIfEnabled(pod)

	// startProbeIfEnabled launches a background goroutine that reads the tracked
	// pod via GetPod; guard the status writes so they don't race its DeepCopy.
	p.mu.Lock()
	pod.Status.Phase = corev1.PodRunning
	now := metav1.Now()
	pod.Status.StartTime = &now
	p.mu.Unlock()
	p.refreshStatus(ctx, pod)
	p.notify(pod)
	// Cloned defers Ready to runPostCloneSetup; Windows+static defers to applyWindowsStaticIP;
	// restore defers to dispatchHibernateRestore.
	if !cloned && !willRunSAC && !restoring && !p.lifecycleAlreadyFailed(pod) {
		p.markLifecycleState(ctx, pod, meta.LifecycleStateReady, "")
	}
	metrics.PodLifecycleTotal.WithLabelValues("create", "ok", "").Inc()
	return nil
}

// bringUpVM dispatches on mode: unmanaged, clone, run, or fork. The
// returned sourceImage feeds post-clone classification.
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
	case meta.ReadRestoreFromHibernate(pod):
		sourceName, snapshot, err := p.resolveWakeSource(ctx, spec.VMName)
		if err != nil {
			return nil, "", err
		}
		v, err := p.cloneFromHibernate(ctx, spec, sourceName, snapshot)
		if err != nil {
			return nil, "", err
		}
		return v, "", nil

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
			OnDemand:   useOnDemandClone(spec.OS),
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
			OnDemand:   useOnDemandClone(spec.OS),
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
			Backend:    backend,
			NoDirectIO: noDirectIO,
		})
		if err != nil {
			return nil, "", fmt.Errorf("run vm %s: %w", spec.VMName, err)
		}
		// Invalidate fork snapshot from a previous incarnation so later
		// sub-agents clone from current state.
		forkName := forkSnapshotName(spec.VMName)
		if err := p.Runtime.SnapshotRemoveIfExists(ctx, forkName); err != nil {
			log.WithFunc("Provider.bringUpVM").Errorf(ctx, err, "invalidate fork snapshot %s", forkName)
		}
		return v, "", nil

	default: // clone is the default
		repo, tag := ociutil.ParseRef(spec.Image)
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
		// cocoon's `vm clone --pull` only fetches http(s) bases; an OCI-ref base
		// must be materialized here. Dedup by digest — the same bytes may be
		// local under another name (epoch→AR ref migration).
		if srcImage != "" && !isHTTPURL(srcImage) && !p.imagePresent(ctx, snapshot.ImageDigest) {
			if _, imgErr := p.ensureRunImage(ctx, srcImage, false); imgErr != nil {
				return nil, "", fmt.Errorf("ensure clone base image %s: %w", srcImage, imgErr)
			}
		}

		v, err := p.Runtime.Clone(ctx, vm.CloneOptions{
			From:       local,
			To:         spec.VMName,
			Network:    spec.Network,
			Backend:    backend,
			NoDirectIO: noDirectIO,
			Pull:       srcImage != "",
			OnDemand:   useOnDemandClone(spec.OS),
		})
		if err != nil {
			return nil, "", fmt.Errorf("clone vm %s from %s: %w", spec.VMName, local, err)
		}
		return v, srcImage, nil
	}
}

// imagePresent reports whether an image with this digest is in the local store under any name.
func (p *Provider) imagePresent(ctx context.Context, digest string) bool {
	if digest == "" {
		return false
	}
	_, err := p.Runtime.Image(ctx, digest)
	return err == nil
}

// ensureRunImage materializes the base image locally and returns the ref
// `cocoon vm run` should be invoked with. A cloud-image artifact is imported
// into the local store and booted from there (repo:tag), not the registry.
func (p *Provider) ensureRunImage(ctx context.Context, image string, force bool) (string, error) {
	if image == "" {
		return image, nil
	}
	if p.Puller == nil || p.Puller.Registry == nil || isHTTPURL(image) {
		return image, p.Runtime.EnsureImage(ctx, image, force)
	}
	repo, tag := ociutil.ParseRef(image)
	raw, _, err := p.Puller.Registry.GetManifest(ctx, repo, tag)
	if err != nil {
		// Ref absent from the registry or a hiccup; cocoon image pull handles external refs natively.
		return image, p.Runtime.EnsureImage(ctx, image, force)
	}
	kind, classifyErr := manifest.Classify(raw)
	if classifyErr != nil {
		return image, p.Runtime.EnsureImage(ctx, image, force)
	}
	switch kind {
	case manifest.KindCloudImage:
		local := repo + ":" + tag
		return local, p.Puller.EnsureCloudImageFromRaw(ctx, repo, local, raw, force)
	case manifest.KindSnapshot:
		return image, fmt.Errorf("image %s is a snapshot artifact; use mode=clone instead of mode=run", image)
	default:
		return image, p.Runtime.EnsureImage(ctx, image, force)
	}
}

// ensureSnapshot returns the local snapshot, pulling from the registry if needed.
// Local name includes the tag so myvm:v1 and myvm:v2 stay separate.
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

// ensureForkSnapshot returns the fork snapshot for a source VM, creating
// once and reusing thereafter; `snapshot save` pauses the VM and costs
// ~2s/GiB, which would multiply hot-scale by 4-5× if paid per sub-agent.
func (p *Provider) ensureForkSnapshot(ctx context.Context, sourceVMName string) (string, error) {
	snapshotName := forkSnapshotName(sourceVMName)

	// singleflight so concurrent sub-agents forking the same main don't race into
	// SnapshotSave and all but one fail "snapshot name already in use". The shared
	// save is cancel-detached so one caller's aborted CreatePod can't fail the rest.
	created, err, _ := p.forkSnapshotSF.Do(snapshotName, func() (any, error) {
		shared := context.WithoutCancel(ctx)
		if _, err := p.Runtime.Snapshot(shared, snapshotName); err == nil {
			return snapshotName, nil
		}
		sourceVM := p.vmByName(sourceVMName)
		if sourceVM == nil {
			inspected, err := p.Runtime.Inspect(shared, sourceVMName)
			if err != nil {
				return "", fmt.Errorf("inspect fork source vm %s: %w", sourceVMName, err)
			}
			sourceVM = inspected
		}
		if err := p.Runtime.SnapshotSave(shared, snapshotName, sourceVM.ID); err != nil {
			return "", fmt.Errorf("snapshot fork source vm %s as %s: %w", sourceVMName, snapshotName, err)
		}
		return snapshotName, nil
	})
	if err != nil {
		return "", err
	}
	return created.(string), nil
}

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
	var lastErr error
	for range 3 {
		err := p.patchPodAnnotations(ctx, namespace, name, annos)
		if err == nil {
			return
		}
		lastErr = err
		if !commonk8s.SleepCtx(ctx, 500*time.Millisecond) {
			return
		}
	}
	logger.Errorf(ctx, lastErr, "annotation patch failed after retries for %s/%s, will reconcile on restart", namespace, name)
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
	// The readiness probe reads the tracked pod via GetPod (DeepCopy under
	// RLock); guard the write so it doesn't race that copy. GetPodStatus is
	// called before the lock because it RLocks internally.
	p.mu.Lock()
	pod.Status = *status
	p.mu.Unlock()
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

// useOnDemandClone is off for Windows: UFFD lazy paging stalls DHCP boot.
func useOnDemandClone(os string) bool {
	return os != string(cocoonv1.OSWindows)
}

// isClonedBoot reports whether bringUpVM took a clone path. spec.Mode alone
// is insufficient: fromDir / ForkFrom override mode=run for sub-agents.
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

// isHTTPURL reports whether ref looks like an HTTP(S) cloud-image URL.
func isHTTPURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

// localSnapshotName omits the default tag for backward compatibility.
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
