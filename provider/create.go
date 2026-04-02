package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"
)

type createRequest struct {
	key        string
	pod        *corev1.Pod
	mode       string
	spec       podSpecResolution
	runImage   string
	registry   string
	image      string
	storage    string
	nics       string
	dns        string
	rootPwd    string
	osType     string
	loggerFunc string
}

type createPlan struct {
	vmName        string
	requestedMode string
	effectiveMode string
	registryURL   string
	runImage      string
	cloneImage    string
	cpu           string
	mem           string
	storage       string
	nics          string
	dns           string
	rootPwd       string
	osType        string
}

func newCreateRequest(pod *corev1.Pod) createRequest {
	spec := resolvePodSpec(pod)
	return createRequest{
		key:        podKey(pod.Namespace, pod.Name),
		pod:        pod,
		mode:       ann(pod, AnnMode, modeClone),
		spec:       spec,
		runImage:   spec.runImage(),
		registry:   spec.registryURL,
		image:      spec.cloneImage(),
		storage:    spec.storage,
		nics:       spec.nics,
		dns:        spec.dns,
		rootPwd:    spec.rootPwd,
		osType:     spec.osType,
		loggerFunc: "provider.CreatePod",
	}
}

func (p createPlan) cocoonArgs() []string {
	switch p.effectiveMode {
	case modeRun:
		return buildRunArgs(runConfig{
			vmName:  p.vmName,
			cpu:     p.cpu,
			mem:     p.mem,
			storage: p.storage,
			nics:    p.nics,
			dns:     p.dns,
			rootPwd: p.rootPwd,
			image:   p.runImage,
			osType:  p.osType,
		})
	default:
		return buildCloneArgs(p.vmName, p.cloneImage)
	}
}

func (p createPlan) snapshotFrom() string {
	if p.requestedMode == modeRun {
		return p.runImage
	}
	return p.cloneImage
}

func (p createPlan) logImage() string {
	if p.effectiveMode == modeRun {
		return p.runImage
	}
	return p.cloneImage
}

func (c *CocoonProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error { //nolint:gocyclo // pod creation orchestrates multiple subsystems
	req := newCreateRequest(pod)
	logger := log.WithFunc(req.loggerFunc)
	logger.Infof(ctx, "%s", req.key)

	switch req.mode {
	case modeStatic:
		return c.createStaticPod(ctx, req)
	case modeAdopt:
		return c.createAdoptedPod(ctx, req)
	}

	if shouldRecoverManagedPod(req.mode, req.pod) && c.recoverManagedPod(ctx, req.pod, req.key, req.image, req.osType) {
		return nil
	}

	vmName := c.reserveManagedVMName(ctx, req)
	plan := c.buildCreatePlan(ctx, req, vmName)
	c.removeStaleVM(ctx, req.key, vmName)

	vm, err := c.runCreatePlan(ctx, req, plan)
	if err != nil {
		return err
	}

	c.finishCreate(ctx, req, plan, vm)
	return nil
}

func (c *CocoonProvider) createStaticPod(ctx context.Context, req createRequest) error {
	vmIP := ann(req.pod, AnnIP, "")
	if vmIP == "" {
		return fmt.Errorf("static mode requires cocoon.cis/ip annotation")
	}

	vm := &CocoonVM{
		podNamespace: req.pod.Namespace,
		podName:      req.pod.Name,
		vmName:       req.pod.Name,
		vmID:         ann(req.pod, AnnVMID, "static-"+req.pod.Name),
		ip:           vmIP,
		state:        stateRunning,
		os:           req.osType,
		managed:      false,
		cpu:          2,
		memoryMB:     4096,
		createdAt:    time.Now(),
		startedAt:    time.Now(),
	}
	c.storePodVM(ctx, req.key, req.pod, vm)
	log.WithFunc(req.loggerFunc).Infof(ctx, "%s: static VM ip=%s os=%s", req.key, vm.ip, vm.os)
	go c.notifyPodStatus(ctx, req.pod.Namespace, req.pod.Name)
	return nil
}

func (c *CocoonProvider) createAdoptedPod(ctx context.Context, req createRequest) error {
	adoptName := ann(req.pod, AnnVMName, "")
	adoptID := ann(req.pod, AnnVMID, "")

	vm := c.lookupRecoverableVM(ctx, adoptID, adoptName)
	if vm == nil {
		return fmt.Errorf("adopt: VM not found (name=%q id=%q)", adoptName, adoptID)
	}

	vm.podNamespace = req.pod.Namespace
	vm.podName = req.pod.Name
	vm.managed = ann(req.pod, AnnManaged, "false") == valTrue
	vm.os = req.osType
	if ipAnn := ann(req.pod, AnnIP, ""); ipAnn != "" {
		vm.ip = ipAnn
	}
	if vm.ip == "" {
		vm.ip = c.resolveIPFromLease(vm.vmName)
	}

	c.storePodVM(ctx, req.key, req.pod, vm)
	log.WithFunc(req.loggerFunc).Infof(ctx, "%s: adopted existing VM %s (%s) ip=%s", req.key, vm.vmName, vm.vmID, vm.ip)
	return nil
}

func (c *CocoonProvider) reserveManagedVMName(ctx context.Context, req createRequest) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	vmName := c.deriveStableVMNameLocked(ctx, req.pod)
	c.vms[req.key] = &CocoonVM{vmName: vmName, state: stateCreating, managed: true}
	return vmName
}

func (c *CocoonProvider) buildCreatePlan(ctx context.Context, req createRequest, vmName string) createPlan {
	cloneImage, registryURL := c.resolveCloneSource(ctx, req, vmName)
	cpu, mem := podResourceLimits(req.pod)

	plan := createPlan{
		vmName:        vmName,
		requestedMode: req.mode,
		effectiveMode: req.mode,
		registryURL:   registryURL,
		runImage:      req.runImage,
		cloneImage:    cloneImage,
		cpu:           cpu,
		mem:           mem,
		storage:       req.storage,
		nics:          req.nics,
		dns:           req.dns,
		rootPwd:       req.rootPwd,
		osType:        req.osType,
	}
	c.applyEpochCreateSource(ctx, req, &plan)
	return plan
}

func (c *CocoonProvider) resolveCloneSource(ctx context.Context, req createRequest, vmName string) (string, string) {
	cloneImage := req.image
	registryURL := req.registry
	if req.mode != modeClone {
		return cloneImage, registryURL
	}

	logger := log.WithFunc(req.loggerFunc)
	slot := extractSlotFromVMName(vmName)
	snapshots := c.snapshotManager()

	if suspended, ok := snapshots.consumeSuspendedSnapshot(ctx, req.pod.Namespace, vmName, slot == 0); ok {
		logger.Infof(ctx, "%s: restoring from suspended snapshot %s", req.key, suspended.ref)
		cloneImage = suspended.snapshot
		if suspended.registryURL != "" {
			registryURL = suspended.registryURL
		}
		return cloneImage, registryURL
	}

	if forkSource := ann(req.pod, AnnForkFrom, ""); forkSource != "" {
		if forkSnap := c.forkFromVM(ctx, req.pod.Namespace, forkSource, vmName); forkSnap != "" {
			logger.Infof(ctx, "%s: forking from %s (CocoonSet annotation), snapshot %s", req.key, forkSource, forkSnap)
			return forkSnap, registryURL
		}
	}

	if slot > 0 {
		if forkSnap := c.forkFromMainAgent(ctx, req.pod.Namespace, vmName); forkSnap != "" {
			logger.Infof(ctx, "%s: forking sub-agent from slot-0 snapshot %s", req.key, forkSnap)
			return forkSnap, registryURL
		}
	}

	return cloneImage, registryURL
}

func (c *CocoonProvider) removeStaleVM(ctx context.Context, key, vmName string) {
	if existing := c.discoverVM(ctx, vmName); existing != nil && existing.vmID != "" {
		log.WithFunc("provider.CreatePod").Infof(ctx, "%s: stale VM %s exists (%s), removing", key, vmName, existing.state)
		c.removeVM(ctx, existing.vmID)
	}
}

func (c *CocoonProvider) applyEpochCreateSource(ctx context.Context, req createRequest, plan *createPlan) {
	puller := c.getPuller(ctx, plan.registryURL)
	if puller == nil { //nolint:nestif // mode-specific epoch handling
		return
	}

	logger := log.WithFunc(req.loggerFunc)
	switch req.mode {
	case modeClone:
		if err := puller.EnsureSnapshot(ctx, plan.cloneImage); err != nil {
			logger.Warnf(ctx, "%s: epoch pull %s failed (will try local): %v", req.key, plan.cloneImage, err)
		}
	case modeRun:
		if req.osType == osWindows {
			if err := puller.EnsureCloudImage(ctx, req.image); err != nil {
				logger.Warnf(ctx, "%s: epoch cloud image import %s failed (will try direct run): %v", req.key, req.image, err)
			} else {
				plan.runImage = req.image
			}
			return
		}
		if err := puller.EnsureSnapshot(ctx, req.image); err != nil {
			logger.Warnf(ctx, "%s: epoch pull %s failed (will try direct run): %v", req.key, req.image, err)
			return
		}
		plan.effectiveMode = modeClone
		plan.cloneImage = req.image
	}
}

func (c *CocoonProvider) runCreatePlan(ctx context.Context, req createRequest, plan createPlan) (*CocoonVM, error) {
	out, err := c.runPlanCommand(ctx, plan)
	if err != nil {
		log.WithFunc(req.loggerFunc).Errorf(ctx, err, "%s: %s", req.key, out)
		c.releaseReservedVM(req.key)
		return nil, fmt.Errorf("cocoon %s: %w", plan.effectiveMode, err)
	}

	log.WithFunc(req.loggerFunc).Infof(ctx, "%s: cocoon %s OK (requested=%s image=%s)", req.key, plan.effectiveMode, req.mode, plan.logImage())
	vm := c.discoverCreatedVM(ctx, plan.vmName, parseVMID(out))
	if vm == nil {
		vm = fallbackCreatedVM(plan)
	}
	return vm, nil
}

func (c *CocoonProvider) runPlanCommand(ctx context.Context, plan createPlan) (string, error) {
	if plan.effectiveMode == modeClone {
		out, err := c.cocoonExec(ctx, buildCloneArgs(plan.vmName, plan.cloneImage)...)
		if err == nil {
			return out, nil
		}

		resolved := resolveCloneBootImage(plan.cloneImage)
		if resolved == plan.cloneImage {
			return out, err
		}
		return c.cocoonExec(ctx, buildRunArgs(runConfig{
			vmName:  plan.vmName,
			cpu:     plan.cpu,
			mem:     plan.mem,
			storage: plan.storage,
			image:   resolved,
			osType:  plan.osType,
		})...)
	}

	out, err := c.cocoonExec(ctx, plan.cocoonArgs()...)
	if err == nil {
		return out, nil
	}
	return c.cocoonExec(ctx, buildLegacyRunArgs(runConfig{
		vmName:  plan.vmName,
		cpu:     plan.cpu,
		mem:     plan.mem,
		storage: plan.storage,
		nics:    plan.nics,
		dns:     plan.dns,
		rootPwd: plan.rootPwd,
		image:   plan.runImage,
		osType:  plan.osType,
	})...)
}

func (c *CocoonProvider) releaseReservedVM(key string) {
	c.mu.Lock()
	delete(c.vms, key)
	c.mu.Unlock()
}

func (c *CocoonProvider) discoverCreatedVM(ctx context.Context, vmName, vmID string) *CocoonVM {
	for range 5 {
		if vmID != "" {
			if vm := c.discoverVMByID(ctx, vmID); vm != nil {
				return vm
			}
		}
		vm := c.discoverVM(ctx, vmName)
		if vm != nil && vm.vmID != "" {
			return vm
		}
		time.Sleep(2 * time.Second)
	}
	return nil
}

func (c *CocoonProvider) finishCreate(ctx context.Context, req createRequest, plan createPlan, vm *CocoonVM) {
	logger := log.WithFunc(req.loggerFunc)
	logger.Infof(ctx, "%s: discovered VM %s (vmid=%s)", req.key, plan.vmName, vm.vmID)

	vm.podNamespace = req.pod.Namespace
	vm.podName = req.pod.Name
	vm.image = req.image
	if req.mode == modeRun {
		vm.image = req.runImage
	}
	vm.os = req.osType
	vm.managed = true
	vm.createdAt = time.Now()
	vm.startedAt = time.Now()
	vm.ip = c.waitForDHCPIP(ctx, vm, 120*time.Second)

	c.storePodVM(ctx, req.key, req.pod, vm, podAnnotation{key: AnnSnapshotFrom, value: plan.snapshotFrom()})
	go c.postBootInject(ctx, req.pod, vm)
	go c.startProbes(ctx, req.pod, vm)
	go c.notifyPodStatus(ctx, req.pod.Namespace, req.pod.Name)
}

func fallbackCreatedVM(plan createPlan) *CocoonVM {
	return &CocoonVM{
		vmName:   plan.vmName,
		state:    stateRunning,
		cpu:      parseCPUString(plan.cpu),
		memoryMB: parseMemoryStringMB(plan.mem),
	}
}
