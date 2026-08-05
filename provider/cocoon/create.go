package cocoon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	"github.com/cocoonstack/cocoon-common/manifest"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/ociutil"

	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	// importDetachTimeout backstops a shared import flight: it outlives every
	// individual caller, so a stalled registry stream needs a deadline of its own.
	importDetachTimeout = 30 * time.Minute

	// cpuPeriodUs is the cpu.max period the quota is computed against;
	// passed alongside the quota so the math never drifts from cocoon's default.
	cpuPeriodUs = 100000
	// minQuotaUs is the kernel's cpu.max floor; kubelet clamps sub-10m limits the same way.
	minQuotaUs = 1000

	// kubelet's cgroup v2 shares→weight conversion bounds.
	minCPUShares = 2
	maxCPUShares = 262144
)

func (p *Provider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.WithFunc("Provider.CreatePod")
	logger.Infof(ctx, "create pod %s/%s", pod.Namespace, pod.Name)

	spec := meta.ParseVMSpec(pod)
	if spec.VMName == "" {
		metrics.PodLifecycleTotal.WithLabelValues("create", "skipped", "missing_vmname").Inc()
		return fmt.Errorf("pod %s/%s missing %s annotation", pod.Namespace, pod.Name, meta.AnnotationVMName)
	}

	// Claim this incarnation before the long bring-up so a forgotten predecessor's stale writers hit the UID guards.
	p.trackPod(pod, nil)
	p.markLifecycleState(ctx, pod, meta.LifecycleStateCreating, "")

	if existing := p.vmByName(spec.VMName); existing != nil {
		p.applyRuntime(ctx, pod, existing)
		p.trackPod(pod, existing)
		p.startProbeIfEnabled(pod)
		p.markReadyPublished(ctx, pod)
		metrics.PodLifecycleTotal.WithLabelValues("create", "ok", "adopted").Inc()
		return nil
	}

	// A restore reuses wake()'s post-restore path (CH+Windows waits on the fresh
	// NIC's lease; others run runPostCloneSetup) and skips the base-image post-clone.
	restoring := meta.ReadRestoreFromHibernate(pod)
	if !restoring && spec.Managed {
		derived, err := p.deriveRestoreFromEvidence(ctx, pod, spec)
		if err != nil {
			return p.failCreate(ctx, pod, false, "HibernateEvidenceFailed", err)
		}
		restoring = derived
	}
	bootStart := time.Now()
	v, sourceImage, err := p.bringUpVM(ctx, pod, spec)
	if err != nil {
		return p.failCreate(ctx, pod, restoring, "CreateBringUpFailed", err)
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
	if spec.OS == string(cocoonv1.OSWindows) && !restoring && !cloned {
		p.goBackground(func() {
			ran, ok := p.runWindowsSAC(p.lifecycleCtx, pod, v, "create")
			// Non-clone Ready was deferred to here so watchers don't see a transient Ready.
			if ok && ran && !p.lifecycleAlreadyFailed(pod) {
				p.markReadyPublished(p.lifecycleCtx, pod)
			}
		})
	}
	if cloned && !restoring {
		p.goBackground(func() {
			p.runPostCloneSetup(p.lifecycleCtx, pod, spec, v, sourceImage, "create", false)
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
	// Cloned defers Ready to runPostCloneSetup; Windows+static defers to applyWindowsStaticIP;
	// restore defers to dispatchHibernateRestore.
	if !cloned && !willRunSAC && !restoring && !p.lifecycleAlreadyFailed(pod) {
		p.markReadyPublished(ctx, pod)
	} else {
		p.refreshStatus(ctx, pod)
		p.notify(pod)
	}
	metrics.PodLifecycleTotal.WithLabelValues("create", "ok", "").Inc()
	return nil
}

// failCreate drops the provisional claim (keep intent) so the kubelet's create
// retry starts clean; a failed restore also counts as a wake failure.
func (p *Provider) failCreate(ctx context.Context, pod *corev1.Pod, restoring bool, reason string, err error) error {
	if restoring {
		metrics.WakeTotal.WithLabelValues("failed").Inc()
	}
	p.failOp(ctx, pod, reason, "create", err)
	p.mu.Lock()
	if tracked := p.pods[meta.PodKey(pod.Namespace, pod.Name)]; tracked != nil && tracked.UID == pod.UID {
		delete(p.pods, meta.PodKey(pod.Namespace, pod.Name))
	}
	p.mu.Unlock()
	return err
}

// deriveRestoreFromEvidence re-derives a wake lost to a vk restart: the pod
// looks freshly creatable, but fresh-booting a name whose guest state sits in
// a hibernate snapshot lets the next hibernate overwrite it (#54).
// classifyNICRecovery (resume.go) relies on these gates running pre-bringUpVM.
func (p *Provider) deriveRestoreFromEvidence(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec) (bool, error) {
	evidence, recordedImage, err := p.hibernateEvidence(ctx, spec.VMName)
	if err != nil {
		metrics.HibernateEvidenceTotal.WithLabelValues("unavailable").Inc()
		p.emitWarningf(pod, "HibernateEvidenceUnavailable", "%v", err)
		return false, err
	}
	if !evidence {
		return false, nil
	}
	if hasExplicitCloneSource(pod, spec) {
		metrics.HibernateEvidenceTotal.WithLabelValues("source_conflict").Inc()
		return false, fmt.Errorf("vm %s has a hibernate snapshot but the pod requests an explicit clone source; delete the %s tag to discard the hibernated state", spec.VMName, meta.HibernateSnapshotTag)
	}
	if recordedImage != "" && recordedImage != spec.Image {
		metrics.HibernateEvidenceTotal.WithLabelValues("image_conflict").Inc()
		return false, fmt.Errorf("hibernate snapshot of vm %s came from image %q but the pod requests %q; delete the %s tag to discard the hibernated state", spec.VMName, recordedImage, spec.Image, meta.HibernateSnapshotTag)
	}
	metrics.HibernateEvidenceTotal.WithLabelValues("restored").Inc()
	// The pod is tracked: GetPod's DeepCopy may be reading this map.
	p.mu.Lock()
	meta.MarkRestoreFromHibernate(pod)
	p.mu.Unlock()
	p.emitWarningf(pod, "HibernateSnapshotExists", "restoring vm %s from its hibernate snapshot instead of fresh-booting", spec.VMName)
	return true, nil
}

// hibernateEvidence reports whether a hibernate snapshot owns vmName's guest
// state, plus the pod image recorded at push time ("" on legacy pushes).
// Registry errors fail closed: never fresh-boot on uncertainty.
func (p *Provider) hibernateEvidence(ctx context.Context, vmName string) (bool, string, error) {
	if p.Registry == nil {
		if _, err := p.Runtime.Snapshot(ctx, vmName); err != nil {
			if errors.Is(err, vm.ErrSnapshotNotFound) {
				return false, "", nil
			}
			return false, "", fmt.Errorf("inspect local snapshot %s: %w", vmName, err)
		}
		return true, "", nil
	}
	m, ok, err := p.fetchHibernateManifest(ctx, vmName)
	if err != nil {
		return false, "", fmt.Errorf("hibernate evidence for %s: %w (refusing fresh boot)", vmName, err)
	}
	if !ok {
		return false, "", nil
	}
	return true, m.Annotations[manifest.AnnotationSnapshotBaseImage], nil
}

// bringUpVM boots the VM for its mode; the returned sourceImage feeds post-clone classification.
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
	policy := podCPUPolicy(pod)
	fromDir, err := parseCloneFromDirAnnotation(pod)
	if err != nil {
		return nil, "", err
	}
	if meta.ReadRestoreFromHibernate(pod) {
		src, err := p.resolveWakeSource(ctx, spec.VMName)
		if err != nil {
			return nil, "", err
		}
		v, err := p.cloneFromHibernate(ctx, spec, src, policy)
		if err != nil {
			return nil, "", err
		}
		return v, "", nil
	}

	// Invalidate the fork snapshot from a previous incarnation so sub-agents clone from current state.
	forkName := forkSnapshotName(spec.VMName)
	if err := p.Runtime.SnapshotRemoveIfExists(ctx, forkName); err != nil {
		log.WithFunc("Provider.bringUpVM").Errorf(ctx, err, "invalidate fork snapshot %s", forkName)
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
			FromDir:     fromDir,
			To:          spec.VMName,
			Network:     spec.Network,
			Backend:     backend,
			NoDirectIO:  noDirectIO,
			RestoreMode: restoreModeFor(p.RestoreMode, spec.OS),
			CPUPolicy:   policy,
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
			From:        cloneFrom,
			To:          spec.VMName,
			Network:     spec.Network,
			Backend:     backend,
			NoDirectIO:  noDirectIO,
			RestoreMode: restoreModeFor(p.RestoreMode, spec.OS),
			CPUPolicy:   policy,
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
			CPUPolicy:  policy,
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
		if baseErr := p.ensureSnapshotBaseImage(ctx, snapshot); baseErr != nil {
			return nil, "", baseErr
		}

		v, err := p.Runtime.Clone(ctx, vm.CloneOptions{
			From:        local,
			To:          spec.VMName,
			Network:     spec.Network,
			Backend:     backend,
			NoDirectIO:  noDirectIO,
			Pull:        srcImage != "",
			RestoreMode: restoreModeFor(p.RestoreMode, spec.OS),
			CPUPolicy:   policy,
		})
		if err != nil {
			return nil, "", fmt.Errorf("clone vm %s from %s: %w", spec.VMName, local, err)
		}
		return v, srcImage, nil
	}
}

// imagePresent reports whether an image with this digest is in the local store under any name.
func (p *Provider) imagePresent(ctx context.Context, digest string) bool {
	return digest != "" && p.Runtime.Image(ctx, digest) == nil
}

// cocoon's `vm clone --pull` only fetches http(s) bases, so an OCI-ref base
// must be imported here. Dedup by digest — the same bytes may be local under
// another name (epoch→AR ref migration).
func (p *Provider) ensureSnapshotBaseImage(ctx context.Context, snapshot *vm.Snapshot) error {
	if snapshot == nil || snapshot.Image == "" || isHTTPURL(snapshot.Image) || p.imagePresent(ctx, snapshot.ImageDigest) {
		return nil
	}
	if _, err := p.ensureRunImage(ctx, snapshot.Image, false); err != nil {
		return fmt.Errorf("ensure snapshot base image %s: %w", snapshot.Image, err)
	}
	return nil
}

// detachedImportContext scopes shared import work to the provider, not to the
// caller that happened to start it: Close aborts it, one CreatePod cannot.
func (p *Provider) detachedImportContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(p.lifecycleCtx, importDetachTimeout)
}

// ensureRunImage materializes the base image locally and returns the ref
// `cocoon vm run` should be invoked with. A cloud-image artifact is imported
// into the local store and booted from there (repo:tag), not the registry.
// The flight key carries force so a force caller never coalesces onto a
// non-force flight that may have been a pure local-store hit; a later force
// call re-imports because the key is evicted when its flight returns.
func (p *Provider) ensureRunImage(ctx context.Context, image string, force bool) (string, error) {
	if image == "" {
		return image, nil
	}
	// Raw ref, not ParseRef-normalized: fallback branches return the leader's
	// spelling verbatim, so a joiner must share its exact ref.
	key := image
	if force {
		key = "force " + image
	}
	ch := p.runImageSF.DoChan(key, func() (any, error) {
		shared, cancel := p.detachedImportContext()
		defer cancel()
		return p.resolveRunImage(shared, image, force)
	})
	return awaitFlight(ctx, ch, image)
}

func (p *Provider) resolveRunImage(ctx context.Context, image string, force bool) (string, error) {
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
	if snapshot, err := p.Runtime.Snapshot(ctx, local); err == nil {
		return snapshot, nil
	}
	if p.Puller == nil {
		return nil, nil
	}
	// The in-flight re-check is load-bearing: SnapshotImport rm's the target
	// name first and cocoon registers it only when the import completes, so a
	// caller that missed the outer check during another flight's pull would
	// otherwise re-import and rm the snapshot that flight just registered.
	ch := p.snapshotPullSF.DoChan(local, func() (any, error) {
		shared, cancel := p.detachedImportContext()
		defer cancel()
		if snapshot, err := p.Runtime.Snapshot(shared, local); err == nil {
			return snapshot, nil
		}
		pullStart := time.Now()
		if pullErr := p.Puller.PullSnapshot(shared, repo, tag, local); pullErr != nil {
			return nil, pullErr
		}
		metrics.SnapshotPullDuration.Observe(time.Since(pullStart).Seconds())
		snapshot, err := p.Runtime.Snapshot(shared, local)
		if err != nil {
			return nil, fmt.Errorf("inspect imported snapshot %s: %w", local, err)
		}
		return snapshot, nil
	})
	return awaitFlight(ctx, ch, (*vm.Snapshot)(nil))
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

// applyRuntime writes VMID/IP annotations onto the in-memory pod (under
// p.mu against GetPod's DeepCopy) and patches them back to the API server.
func (p *Provider) applyRuntime(ctx context.Context, pod *corev1.Pod, v *vm.VM) {
	p.mu.Lock()
	meta.VMRuntime{VMID: v.ID, IP: v.IP}.Apply(pod)
	p.mu.Unlock()
	p.patchRuntimeAnnotations(ctx, pod.Namespace, pod.Name, v)
}

func (p *Provider) patchRuntimeAnnotations(ctx context.Context, namespace, name string, v *vm.VM) {
	annos := map[string]any{
		meta.AnnotationVMID: v.ID,
		meta.AnnotationIP:   v.IP,
	}
	if err := patchWithRetry(ctx, func() error {
		return p.patchPodAnnotations(ctx, namespace, name, annos)
	}); err != nil {
		log.WithFunc("Provider.patchRuntimeAnnotations").
			Errorf(ctx, err, "annotation patch failed after retries for %s/%s, will reconcile on restart", namespace, name)
	}
}

func (p *Provider) startProbeIfEnabled(pod *corev1.Pod) {
	if p.Probes == nil || pod.DeletionTimestamp != nil {
		return
	}
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.Probes.Start(key, p.buildProbe(pod.Namespace, pod.Name), p.buildOnUpdate(pod.Namespace, pod.Name))
}

func (p *Provider) refreshStatus(ctx context.Context, pod *corev1.Pod) {
	status, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name)
	if err != nil {
		return
	}
	// The readiness probe reads the tracked pod via GetPod (DeepCopy under
	// RLock); guard the write so it doesn't race that copy. GetPodStatus is
	// called before the lock because it RLocks internally.
	p.mu.Lock()
	pod.Status = *status
	p.mu.Unlock()
}

// awaitFlight waits on a singleflight result or the caller's cancellation,
// whichever comes first; a canceled caller abandons the flight, which keeps
// running for its remaining waiters.
func awaitFlight[T any](ctx context.Context, ch <-chan singleflight.Result, zero T) (T, error) {
	select {
	case res := <-ch:
		if res.Err != nil {
			return zero, res.Err
		}
		return res.Val.(T), nil
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func parseCloneFromDirAnnotation(pod *corev1.Pod) (string, error) {
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

// Windows forces copy: lazy memory restore stalls DHCP boot.
func restoreModeFor(mode vm.RestoreMode, os string) vm.RestoreMode {
	if os == string(cocoonv1.OSWindows) {
		return vm.RestoreCopy
	}
	return mode
}

// isClonedBoot reports whether bringUpVM took a clone path. spec.Mode alone
// is insufficient: fromDir / ForkFrom override mode=run for sub-agents.
func isClonedBoot(pod *corev1.Pod, spec meta.VMSpec) bool {
	return hasExplicitCloneSource(pod, spec) || strings.ToLower(spec.Mode) != string(cocoonv1.AgentModeRun)
}

func hasExplicitCloneSource(pod *corev1.Pod, spec meta.VMSpec) bool {
	return strings.TrimSpace(pod.Annotations[meta.AnnotationCloneFromDir]) != "" || spec.ForkFrom != ""
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

func isHTTPURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

// localSnapshotName omits the default tag for backward compatibility.
func localSnapshotName(repo, tag string) string {
	name := repo
	if tag != "" && tag != meta.DefaultSnapshotTag {
		name = repo + ":" + tag
	}
	const maxSnapshotNameLength = 63
	if len(name) <= maxSnapshotNameLength {
		return name
	}
	suffix := "-" + ociutil.SHA256Hex([]byte(name))[:12]
	return name[:maxSnapshotNameLength-len(suffix)] + suffix
}

func forkSnapshotName(sourceVMName string) string {
	return "fork-" + sourceVMName
}

// vmResourceOverrides translates pod resources into cocoon CLI args. vCPU
// rounds the CPU limit up (requests when unset): the count is a hard cap
// under cocoon's cgroup scope, so it must bound at the limit.
func vmResourceOverrides(pod *corev1.Pod) (int, string) {
	if len(pod.Spec.Containers) == 0 {
		return 0, ""
	}
	resources := pod.Spec.Containers[0].Resources
	cpu := selectQuantity(resources.Limits, resources.Requests, corev1.ResourceCPU)
	memory := selectQuantity(resources.Requests, resources.Limits, corev1.ResourceMemory)
	return quantityCPURoundUp(cpu), quantityArg(memory)
}

// podCPUPolicy derives cgroup knobs: quota caps at the CPU limit and
// never falls back to requests, weight always tracks requests (limits
// when unset, mirroring the K8s Guaranteed defaulting) so a BestEffort
// pod gets kubelet's minimum share, not cocoon's vCPU-count default.
func podCPUPolicy(pod *corev1.Pod) vm.CPUPolicy {
	if len(pod.Spec.Containers) == 0 {
		return vm.CPUPolicy{}
	}
	resources := pod.Spec.Containers[0].Resources
	var policy vm.CPUPolicy
	if limit := resources.Limits[corev1.ResourceCPU]; !limit.IsZero() {
		policy.CPUQuotaUs = max(limit.MilliValue()*cpuPeriodUs/1000, minQuotaUs) //nolint:mnd // millicores per core
		policy.CPUPeriodUs = cpuPeriodUs
	}
	request := selectQuantity(resources.Requests, resources.Limits, corev1.ResourceCPU)
	policy.CPUWeight = cpuWeightFromMilli(request.MilliValue())
	return policy
}

// cpuWeightFromMilli mirrors kubelet's cgroup v2 conversion so a VM's
// contention share matches what kubelet grants the same requests.
func cpuWeightFromMilli(milli int64) int {
	shares := min(max(milli*1024/1000, minCPUShares), maxCPUShares)
	return int(1 + (shares-minCPUShares)*9999/(maxCPUShares-minCPUShares))
}

func selectQuantity(primary, fallback corev1.ResourceList, name corev1.ResourceName) resource.Quantity {
	if q, ok := primary[name]; ok && !q.IsZero() {
		return q
	}
	if q, ok := fallback[name]; ok && !q.IsZero() {
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

// quantityArg renders a non-zero quantity as-is; vm's normalizeSizeArg
// owns the byte conversion for cocoon's CLI flags.
func quantityArg(q resource.Quantity) string {
	if q.IsZero() {
		return ""
	}
	return q.String()
}
