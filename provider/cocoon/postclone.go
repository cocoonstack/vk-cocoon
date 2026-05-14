package cocoon

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	cocoonv1 "github.com/cocoonstack/cocoon-common/apis/v1"
	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/guest"
	"github.com/cocoonstack/vk-cocoon/guest/sac"
	"github.com/cocoonstack/vk-cocoon/metrics"
	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	annotationPostCloneHint   = "vm.cocoonstack.io/post-clone-hint"
	annotationPostCloneState  = "vm.cocoonstack.io/post-clone-state"
	annotationPostCloneErrors = "vm.cocoonstack.io/post-clone-errors"

	postCloneStateRunning = "running"
	postCloneStateDone    = "done"
	postCloneStateFailed  = "failed"

	postCloneAgentBudget    = 180 * time.Second
	postCloneRetryInterval  = 3 * time.Second
	postCloneErrorsMaxBytes = 4096

	postCloneKindWindows     = "windows"
	postCloneKindLinuxStatic = "linux_static"
	postCloneKindLinuxFC     = "linux_fc"

	sacEnumRetries  = 60 // polls SAC `i` until Windows PnP lists every NIC
	sacIPSetRetries = 10 // per-NIC retry until SAC `i` reflects the assigned IP
)

// runPostCloneSetup auto-executes the post-clone fixup inside the cloned
// guest via cocoon-agent vsock and records the outcome in
// vm.cocoonstack.io/post-clone-state. On timeout or failure it falls back
// to writing the post-clone-hint annotation so an operator has a manual path.
func (p *Provider) runPostCloneSetup(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, v *vm.VM, sourceImage, op string) {
	plan, ok := planPostClone(spec, v, sourceImage)
	if !ok {
		// No fixup required — DHCP self-heals on CH+OCI/cloudimg.
		p.markLifecycleState(ctx, pod, meta.LifecycleStateReady, "")
		return
	}
	logger := log.WithFunc("Provider.runPostCloneSetup")
	kind := postCloneKind(spec)
	t0 := time.Now()
	logger.Infof(ctx, "post-clone setup starting for %s/%s vm=%s kind=%s",
		pod.Namespace, pod.Name, v.ID, kind)
	p.markPostCloneState(ctx, pod, postCloneStateRunning)

	// SIGKILL to the sudo wrapper from exec.CommandContext doesn't
	// propagate to the grandchild cocoon process, so cmd.Wait can block
	// on its open stdout pipe. Drive each Exec in a worker and select
	// on the deadline so we abandon stuck workers instead of inheriting
	// their hang.
	loopCtx, cancel := context.WithTimeout(ctx, postCloneAgentBudget)
	defer cancel()

	var attemptErrs []error
	for attempt := 1; ; attempt++ {
		attemptStart := time.Now()
		done := make(chan error, 1)
		go func() {
			done <- p.Runtime.Exec(loopCtx, v.ID, plan.argv, nil, nil, io.Discard, io.Discard)
		}()
		select {
		case execErr := <-done:
			if execErr == nil {
				metrics.PostCloneTotal.WithLabelValues(kind, "ok").Inc()
				metrics.PostCloneRetryAttempts.WithLabelValues("ok").Observe(float64(attempt))
				logger.Infof(ctx, "post-clone setup succeeded for %s/%s vm=%s attempts=%d attempt_dur=%s total_dur=%s",
					pod.Namespace, pod.Name, v.ID, attempt, time.Since(attemptStart).Round(time.Millisecond), time.Since(t0).Round(time.Millisecond))
				p.markPostCloneState(ctx, pod, postCloneStateDone)
				// Don't clobber a prior Failed (e.g. applyWindowsStaticIP race) with Ready.
				if !p.lifecycleAlreadyFailed(pod) {
					p.markLifecycleState(ctx, pod, meta.LifecycleStateReady, "")
					p.emitNormalf(pod, "PostCloneSucceeded", "kind=%s attempts=%d", kind, attempt)
				}
				return
			}
			attemptErrs = append(attemptErrs, fmt.Errorf("attempt %d: %w", attempt, execErr))
			p.emitWarningf(pod, "PostCloneExecAttemptFailed", "attempt %d: %v", attempt, execErr)
			logger.Warnf(ctx, "post-clone exec attempt %d failed for %s/%s after %s: %v",
				attempt, pod.Namespace, pod.Name, time.Since(attemptStart).Round(time.Millisecond), execErr)
		case <-loopCtx.Done():
			attemptErrs = append(attemptErrs, fmt.Errorf("attempt %d: %w", attempt, loopCtx.Err()))
		}
		if loopCtx.Err() != nil {
			break
		}
		if !commonk8s.SleepCtx(loopCtx, postCloneRetryInterval) {
			break
		}
	}
	joinedErr := errors.Join(attemptErrs...)
	if errors.Is(joinedErr, context.Canceled) {
		// Provider shutdown canceled lifecycleCtx; the pod will be re-
		// reconciled on next start. Skip the failed-state + hint writes
		// because their patch ctx is the same canceled one.
		logger.Infof(ctx, "post-clone setup canceled for %s/%s after %s (provider shutdown)",
			pod.Namespace, pod.Name, time.Since(t0).Round(time.Millisecond))
		return
	}
	metrics.PostCloneTotal.WithLabelValues(kind, "failed").Inc()
	metrics.PostCloneRetryAttempts.WithLabelValues("exhausted").Observe(float64(len(attemptErrs)))
	p.markPostCloneState(ctx, pod, postCloneStateFailed)
	p.emitPostCloneHint(ctx, pod, spec, v, sourceImage, attemptErrs)
	p.failOp(ctx, pod, "PostCloneExecExhausted", op, joinedErr)
}

// postCloneKind classifies a clone for the postclone_total{kind} label.
func postCloneKind(spec meta.VMSpec) string {
	if spec.OS == string(cocoonv1.OSWindows) {
		return postCloneKindWindows
	}
	if spec.Backend == vm.BackendFirecracker {
		return postCloneKindLinuxFC
	}
	return postCloneKindLinuxStatic
}

// postClonePlan carries both the agent-side argv and a shell-runnable
// hint string. The hint mirrors argv but quoted so an operator can paste
// the annotation value into a cocoon vm console session unchanged.
type postClonePlan struct {
	argv []string
	hint string
}

// planPostClone returns the auto-exec plan for a cloned VM, or ok=false
// when no setup is needed: CH+OCI+DHCP self-heals via networkd hot-swap
// and CH+cloudimg+DHCP self-heals via the netplan name-match fallback,
// so only static-IP and FC clones still need intervention. Windows
// always needs a PnP-rebind: chained clones land with NDIS-stuck NICs.
func planPostClone(spec meta.VMSpec, v *vm.VM, sourceImage string) (postClonePlan, bool) {
	if spec.OS == string(cocoonv1.OSWindows) {
		argv := buildWindowsPostCloneArgv()
		// argv[3] is the PowerShell script body; quote it as a single
		// shell arg so operators can paste the hint as-is.
		return postClonePlan{argv: argv, hint: fmt.Sprintf("%s %s %s '%s'", argv[0], argv[1], argv[2], argv[3])}, true
	}
	if !needsPostClone(spec.Backend, v.NetworkConfigs) {
		return postClonePlan{}, false
	}
	script := buildPostCloneCommands(spec.VMName, spec.Backend, v.ID, sourceImage, v.NetworkConfigs)
	return postClonePlan{argv: []string{"sh", "-c", script}, hint: script}, true
}

func (p *Provider) markPostCloneState(ctx context.Context, pod *corev1.Pod, state string) {
	// Don't clobber a concurrent failed write (e.g. applyWindowsStaticIP race).
	// The lifecycle annotation has its own gate via lifecycleAlreadyFailed.
	if state != postCloneStateFailed && pod.Annotations[annotationPostCloneState] == postCloneStateFailed {
		return
	}
	p.setPodAnnotation(ctx, pod, annotationPostCloneState, state)
}

// emitPostCloneHint records the manual-recovery script and the joined per-
// attempt error chain so operators can both retry from the cocoon console
// and inspect what each attempt actually failed on.
func (p *Provider) emitPostCloneHint(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, v *vm.VM, sourceImage string, attemptErrs []error) {
	plan, ok := planPostClone(spec, v, sourceImage)
	if !ok {
		return
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(plan.hint))
	p.setPodAnnotation(ctx, pod, annotationPostCloneHint, encoded)
	if joined := errors.Join(attemptErrs...); joined != nil {
		p.setPodAnnotation(ctx, pod, annotationPostCloneErrors, truncate(joined.Error(), postCloneErrorsMaxBytes))
	}
	log.WithFunc("Provider.emitPostCloneHint").Warnf(ctx,
		"manual post-clone setup required for %s/%s; hint at annotation %s",
		pod.Namespace, pod.Name, annotationPostCloneHint)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// setPodAnnotation writes one annotation locally (under p.mu to serialize
// with GetPod's DeepCopy) and patches it best-effort.
func (p *Provider) setPodAnnotation(ctx context.Context, pod *corev1.Pod, key, val string) {
	p.mu.Lock()
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[key] = val
	p.mu.Unlock()
	if err := p.patchPodAnnotations(ctx, pod.Namespace, pod.Name, map[string]any{key: val}); err != nil {
		log.WithFunc("Provider.setPodAnnotation").Errorf(ctx, err,
			"patch annotation %s for %s/%s", key, pod.Namespace, pod.Name)
	}
}

// needsPostClone reports whether a Linux clone requires manual fixup.
// FC always does (MAC needs re-applying); CH only when at least one NIC
// has a static IP — DHCP self-heals on both OCI and cloudimg paths.
func needsPostClone(backend string, networkConfigs []*vm.NetworkConfig) bool {
	if backend == vm.BackendFirecracker {
		return true
	}
	return slices.ContainsFunc(networkConfigs, isStaticNIC)
}

// buildWindowsPostCloneArgv returns the cocoon-agent argv for the Windows
// PnP-rebind. -PresentOnly is load-bearing: ghost Net PnP entries from
// earlier MAC generations make Disable-PnpDevice return HRESULT 0x80041001
// and abort the pipeline before reaching the real adapter.
func buildWindowsPostCloneArgv() []string {
	// Cache Get-PnpDevice and pipe it twice — Disable/Enable-PnpDevice
	// both accept InstanceId by pipeline, so this is shorter than
	// ForEach-Object and the inter-cmdlet Start-Sleep is unnecessary
	// (Win11 cmdlets block synchronously on kernel unbind/rebind).
	const ps = `$x=Get-PnpDevice -Class Net -PresentOnly;` +
		`$x|Disable-PnpDevice -Confirm:$false;` +
		`$x|Enable-PnpDevice -Confirm:$false`
	return []string{"powershell", "-nop", "-c", ps}
}

func isCloudimg(image string) bool {
	return strings.HasPrefix(image, "http://") || strings.HasPrefix(image, "https://")
}

// isCloudimgVM tells cloudimg from OCI when sourceImage is empty (forkFrom,
// wake) by probing the on-disk overlay format.
func isCloudimgVM(vmID string) bool {
	rootDir := provider.CocoonRootDir()
	path := fmt.Sprintf("%s/run/%s/%s/overlay.qcow2", rootDir, runDirCH, vmID)
	_, err := os.Stat(path)
	return err == nil
}

// applyWindowsStaticIP uses SAC to set static IPs on Windows VMs.
// Called for both run and clone when the network uses IPAM. ran=true only
// when SAC actually executed; the skipped cases (no SAC client, no NICs,
// no static NICs) return (false, nil) so the caller can avoid double-counting
// skips as successes in postclone_total.
func (p *Provider) applyWindowsStaticIP(ctx context.Context, pod *corev1.Pod, v *vm.VM) (bool, error) {
	if p.GuestSAC == nil || len(v.NetworkConfigs) == 0 {
		return false, nil
	}
	if !slices.ContainsFunc(v.NetworkConfigs, isStaticNIC) {
		return false, nil
	}

	logger := log.WithFunc("Provider.applyWindowsStaticIP")
	sockPath := fmt.Sprintf("%s/run/%s/%s/console.sock", provider.CocoonRootDir(), runDirCH, v.ID)

	sess, err := p.GuestSAC.Dial(ctx, sockPath)
	if err != nil {
		p.emitWarningf(pod, "PostCloneSACDialFailed", "%v", err)
		return true, fmt.Errorf("sac dial: %w", err)
	}
	defer func() { _ = sess.Close() }()

	netNums, err := p.sacEnumerateNICs(ctx, pod, sess, len(v.NetworkConfigs))
	if err != nil {
		return true, err
	}

	for i, nc := range v.NetworkConfigs {
		if !isStaticNIC(nc) {
			continue
		}
		if err := p.sacSetNICIP(ctx, pod, sess, netNums[i], nc); err != nil {
			return true, err
		}
	}
	logger.Infof(ctx, "sac configured static IPs for %s/%s", pod.Namespace, pod.Name)
	return true, nil
}

// sacEnumerateNICs polls SAC `i` until Windows PnP lists every NIC, or
// returns an error when the budget is exhausted.
func (p *Provider) sacEnumerateNICs(ctx context.Context, pod *corev1.Pod, sess guest.Session, wantNICs int) ([]int, error) {
	logger := log.WithFunc("Provider.sacEnumerateNICs")
	var out bytes.Buffer
	var netNums []int
	for attempt := range sacEnumRetries {
		out.Reset()
		if queryErr := sess.Run(ctx, []string{"i"}, &out); queryErr != nil {
			logger.Debugf(ctx, "sac query: %v", queryErr)
		} else {
			netNums = sac.ParseNetEntries(out.String())
			if len(netNums) >= wantNICs {
				return netNums, nil
			}
		}
		if attempt == sacEnumRetries-1 {
			err := fmt.Errorf("sac enum exhausted: found %d need %d", len(netNums), wantNICs)
			p.emitWarningf(pod, "PostCloneSACEnumFailed", "%v", err)
			return nil, err
		}
		if !commonk8s.SleepCtx(ctx, 2*time.Second) {
			return nil, ctx.Err()
		}
	}
	return netNums, nil
}

// sacSetNICIP issues the SAC set command and verifies the result with a
// bounded retry loop.
func (p *Provider) sacSetNICIP(ctx context.Context, pod *corev1.Pod, sess guest.Session, netNum int, nc *vm.NetworkConfig) error {
	logger := log.WithFunc("Provider.sacSetNICIP")
	cmd := []string{"i", strconv.Itoa(netNum), nc.Network.IP, prefixToSubnet(nc.Network.Prefix), nc.Network.Gateway}
	var out bytes.Buffer
	for attempt := range sacIPSetRetries {
		if setErr := sess.Run(ctx, cmd, nil); setErr != nil {
			p.emitWarningf(pod, "PostCloneSACSetFailed", "net %d: %v", netNum, setErr)
			return fmt.Errorf("sac set ip net %d: %w", netNum, setErr)
		}
		out.Reset()
		if verifyErr := sess.Run(ctx, []string{"i"}, &out); verifyErr != nil {
			logger.Debugf(ctx, "sac verify: %v", verifyErr)
		} else if sac.NetHasIP(out.String(), netNum, nc.Network.IP) {
			return nil
		}
		if attempt == sacIPSetRetries-1 {
			err := fmt.Errorf("sac verify exhausted: net %d ip %s did not take effect", netNum, nc.Network.IP)
			p.emitWarningf(pod, "PostCloneSACVerifyFailed", "%v", err)
			return err
		}
		if !commonk8s.SleepCtx(ctx, 2*time.Second) {
			return ctx.Err()
		}
	}
	return nil
}

func isStaticNIC(nc *vm.NetworkConfig) bool {
	return nc.Network != nil && nc.Network.IP != ""
}

// prefixToSubnet converts a CIDR prefix length to a dotted-decimal subnet mask.
func prefixToSubnet(prefix int) string {
	if prefix <= 0 || prefix > 32 {
		return "255.255.255.0"
	}
	mask := uint32(0xFFFFFFFF) << (32 - prefix)
	return fmt.Sprintf("%d.%d.%d.%d", mask>>24, (mask>>16)&0xFF, (mask>>8)&0xFF, mask&0xFF)
}

// buildPostCloneCommands generates the shell commands a user must
// execute inside the guest to fix networking after a clone.
// cloudimg VMs use cloud-init to reconfigure; OCI VMs use direct
// systemd-networkd file writes.
func buildPostCloneCommands(vmName, backend, vmID, sourceImage string, networkConfigs []*vm.NetworkConfig) string {
	var cmds []string

	cmds = append(cmds, "echo 3 > /proc/sys/vm/drop_caches")
	cmds = append(cmds, "echo "+vmName+" > /etc/hostname")

	if backend == vm.BackendFirecracker {
		for i, nc := range networkConfigs {
			cmds = append(cmds, fmt.Sprintf(
				"ip link set dev eth%d down && ip link set dev eth%d address %s && ip link set dev eth%d up",
				i, i, nc.MAC, i,
			))
		}
	}

	cmds = append(cmds, "rm -f /etc/systemd/network/10-*.network")

	if isCloudimg(sourceImage) || isCloudimgVM(vmID) {
		cmds = append(cmds, "cloud-init clean --logs --seed --configs network && cloud-init init --local && cloud-init init")
		cmds = append(cmds, "cloud-init modules --mode=config && systemctl restart systemd-networkd")
	} else {
		for _, nc := range networkConfigs {
			cmds = append(cmds, buildNetworkdFileCmd(nc))
		}
		cmds = append(cmds, "systemctl restart systemd-networkd")
	}

	return strings.Join(cmds, "\n")
}

func buildNetworkdFileCmd(nc *vm.NetworkConfig) string {
	macSan := strings.ReplaceAll(nc.MAC, ":", "")
	var cfg string
	if isStaticNIC(nc) {
		cfg = fmt.Sprintf("[Match]\\nMACAddress=%s\\n\\n[Network]\\nAddress=%s/%d\\nGateway=%s\\n",
			nc.MAC, nc.Network.IP, nc.Network.Prefix, nc.Network.Gateway)
	} else {
		cfg = fmt.Sprintf("[Match]\\nMACAddress=%s\\n\\n[Network]\\nDHCP=ipv4\\n\\n[DHCPv4]\\nClientIdentifier=mac\\n",
			nc.MAC)
	}
	return fmt.Sprintf("printf '%s' > /etc/systemd/network/10-%s.network", cfg, macSan)
}
