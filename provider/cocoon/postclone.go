package cocoon

import (
	"bytes"
	"cmp"
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
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	annotationPostCloneHint   = "vm.cocoonstack.io/post-clone-hint"
	annotationPostCloneState  = "vm.cocoonstack.io/post-clone-state"
	annotationPostCloneErrors = "vm.cocoonstack.io/post-clone-errors"

	postCloneStateRunning = "running"
	postCloneStateDone    = "done"
	postCloneStateFailed  = "failed"

	postCloneAgentBudget   = 180 * time.Second
	postCloneRetryInterval = 3 * time.Second

	postCloneKindWindows     = "windows"
	postCloneKindLinuxStatic = "linux_static"
	postCloneKindLinuxFC     = "linux_fc"

	sacEnumRetries  = 60 // polls SAC `i` until Windows PnP lists every NIC
	sacIPSetRetries = 10 // per-NIC retry until SAC `i` reflects the assigned IP
)

// runPostCloneSetup runs the cocoon-agent fixup and records post-clone-state;
// on exhaustion it leaves a manual hint annotation for the operator.
func (p *Provider) runPostCloneSetup(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, v *vm.VM, sourceImage, op string) {
	plan, ok := planPostClone(spec, v, sourceImage)
	if !ok {
		p.markReadyAfterIP(ctx, pod, v)
		return
	}
	logger := log.WithFunc("Provider.runPostCloneSetup")
	kind := postCloneKind(spec)
	t0 := time.Now()
	logger.Infof(ctx, "post-clone setup starting for %s/%s vm=%s kind=%s",
		pod.Namespace, pod.Name, v.ID, kind)
	p.markPostCloneState(ctx, pod, postCloneStateRunning)

	// `vm exec` can hang on a wedged guest; abandon stuck workers on deadline.
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
				if !p.lifecycleAlreadyFailed(pod) {
					p.emitNormalf(pod, "PostCloneSucceeded", "kind=%s attempts=%d", kind, attempt)
					p.markReadyAfterIP(ctx, pod, v)
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
		// Provider shutdown canceled the patch ctx too; pod re-reconciles on next start.
		logger.Infof(ctx, "post-clone setup canceled for %s/%s after %s (provider shutdown)",
			pod.Namespace, pod.Name, time.Since(t0).Round(time.Millisecond))
		return
	}
	metrics.PostCloneTotal.WithLabelValues(kind, "failed").Inc()
	metrics.PostCloneRetryAttempts.WithLabelValues("failed").Observe(float64(len(attemptErrs)))
	p.markPostCloneState(ctx, pod, postCloneStateFailed)
	p.emitPostCloneHint(ctx, pod, spec, v, sourceImage, joinedErr)
	joinedMsg := joinedErr.Error()
	p.emitWarningf(pod, "PostCloneExecExhausted", "%s", truncate(op+": "+joinedMsg, eventMessageMaxBytes))
	p.markLifecycleState(ctx, pod, meta.LifecycleStateFailed, truncate(joinedMsg, lifecycleMessageMaxBytes))
	logger.Errorf(ctx, joinedErr, "%s/%s post-clone exhausted", pod.Namespace, pod.Name)
}

// markReadyAfterIP defers lifecycle-state=ready until the clone's DHCP lease lands,
// the same contract the wake path enforces. ready is the signal vm-service reads the
// pod IP on, but the post-clone exec runs over vsock and can finish first — marking
// ready then would promise an IP resolveVMIP can't yet return. On timeout mark
// failed, never ready-without-IP.
func (p *Provider) markReadyAfterIP(ctx context.Context, pod *corev1.Pod, v *vm.VM) {
	gotIP := p.waitForFreshIP(ctx, pod)
	if ctx.Err() != nil {
		return
	}
	if !gotIP {
		metrics.WakeIPWaitTotal.WithLabelValues("timeout").Inc()
		budget := cmp.Or(p.wakeFreshIPBudget, defaultWakeFreshIPBudget)
		err := fmt.Errorf("clone %s: dhcp lease not observed within %s", v.Name, budget)
		msg := err.Error()
		// wake marker: don't clobber a concurrent hibernate/delete with a clone failure.
		if p.markLifecycleStateForWake(ctx, pod, v.ID, meta.LifecycleStateFailed, truncate(msg, lifecycleMessageMaxBytes)) {
			p.emitWarningf(pod, "PostCloneIPWaitTimeout", "%s", truncate(msg, eventMessageMaxBytes))
			log.WithFunc("Provider.markReadyAfterIP").Errorf(ctx, err, "%s/%s post-clone ip wait timeout", pod.Namespace, pod.Name)
		}
		return
	}
	metrics.WakeIPWaitTotal.WithLabelValues("ok").Inc()
	p.refreshStatus(ctx, pod)
	p.notify(pod)
	// wake marker: don't publish Ready over a VM hibernated/replaced mid-wait.
	p.markLifecycleStateForWake(ctx, pod, v.ID, meta.LifecycleStateReady, "")
}

func (p *Provider) markPostCloneState(ctx context.Context, pod *corev1.Pod, state string) {
	key := meta.PodKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	target := p.pods[key]
	if target == nil {
		target = pod
	}
	if target.Annotations == nil {
		target.Annotations = map[string]string{}
	}
	if state != postCloneStateFailed && target.Annotations[annotationPostCloneState] == postCloneStateFailed {
		p.mu.Unlock()
		return
	}
	target.Annotations[annotationPostCloneState] = state
	p.mu.Unlock()
	if err := p.patchPodAnnotations(ctx, pod.Namespace, pod.Name, map[string]any{annotationPostCloneState: state}); err != nil {
		log.WithFunc("Provider.markPostCloneState").Errorf(ctx, err,
			"patch annotation %s for %s/%s", annotationPostCloneState, pod.Namespace, pod.Name)
	}
}

// emitPostCloneHint writes the manual-recovery script + per-attempt error chain.
func (p *Provider) emitPostCloneHint(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, v *vm.VM, sourceImage string, joinedErr error) {
	plan, ok := planPostClone(spec, v, sourceImage)
	if !ok {
		return
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(plan.hint))
	p.setPodAnnotation(ctx, pod, annotationPostCloneHint, encoded)
	if joinedErr != nil {
		p.setPodAnnotation(ctx, pod, annotationPostCloneErrors, truncate(joinedErr.Error(), lifecycleMessageMaxBytes))
	}
	log.WithFunc("Provider.emitPostCloneHint").Warnf(ctx,
		"manual post-clone setup required for %s/%s; hint at annotation %s",
		pod.Namespace, pod.Name, annotationPostCloneHint)
}

// setPodAnnotation writes one annotation locally under p.mu and patches it best-effort.
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

// willRunSAC mirrors applyWindowsStaticIP's guard so CreatePod can defer Ready.
func (p *Provider) willRunSAC(spec meta.VMSpec, v *vm.VM) bool {
	if spec.OS != string(cocoonv1.OSWindows) || p.GuestSAC == nil {
		return false
	}
	return slices.ContainsFunc(v.NetworkConfigs, isStaticNIC)
}

// applyWindowsStaticIP sets static IPs via SAC. ran=true only when SAC
// actually executed, so the caller can avoid counting skips as successes.
func (p *Provider) applyWindowsStaticIP(ctx context.Context, pod *corev1.Pod, v *vm.VM) (bool, error) {
	if p.GuestSAC == nil || len(v.NetworkConfigs) == 0 {
		return false, nil
	}
	if !slices.ContainsFunc(v.NetworkConfigs, isStaticNIC) {
		return false, nil
	}

	logger := log.WithFunc("Provider.applyWindowsStaticIP")
	sockPath := vmRunPath(runDirCH, v.ID, "console.sock")

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

// sacEnumerateNICs polls SAC `i` until every NIC is listed.
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

// sacSetNICIP sets one NIC's IP and verifies it took effect.
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

func postCloneKind(spec meta.VMSpec) string {
	if spec.OS == string(cocoonv1.OSWindows) {
		return postCloneKindWindows
	}
	if spec.Backend == vm.BackendFirecracker {
		return postCloneKindLinuxFC
	}
	return postCloneKindLinuxStatic
}

// postClonePlan pairs cocoon-agent argv with a shell-quoted hint operators can paste.
type postClonePlan struct {
	argv []string
	hint string
}

// planPostClone returns ok=false when no fixup is needed: CH+DHCP self-heals
// on both OCI and cloudimg paths; only static-IP, FC, and Windows clones need it.
func planPostClone(spec meta.VMSpec, v *vm.VM, sourceImage string) (postClonePlan, bool) {
	if spec.OS == string(cocoonv1.OSWindows) {
		argv := buildWindowsPostCloneArgv()
		return postClonePlan{argv: argv, hint: fmt.Sprintf("%s %s %s '%s'", argv[0], argv[1], argv[2], argv[3])}, true
	}
	if !needsPostClone(spec.Backend, v.NetworkConfigs) {
		return postClonePlan{}, false
	}
	script := buildPostCloneCommands(spec.VMName, spec.Backend, v.ID, sourceImage, v.NetworkConfigs)
	return postClonePlan{argv: []string{"sh", "-c", script}, hint: script}, true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// needsPostClone: FC always (MAC re-apply); CH only with a static-IP NIC.
func needsPostClone(backend string, networkConfigs []*vm.NetworkConfig) bool {
	if backend == vm.BackendFirecracker {
		return true
	}
	return slices.ContainsFunc(networkConfigs, isStaticNIC)
}

// buildWindowsPostCloneArgv: -PresentOnly is load-bearing — ghost Net PnP
// entries make Disable-PnpDevice return 0x80041001 before the real adapter.
func buildWindowsPostCloneArgv() []string {
	const ps = `$x=Get-PnpDevice -Class Net -PresentOnly;` +
		`$x|Disable-PnpDevice -Confirm:$false;` +
		`$x|Enable-PnpDevice -Confirm:$false`
	return []string{"powershell", "-nop", "-c", ps}
}

// isCloudimgVM probes the on-disk overlay when sourceImage is empty (forkFrom, wake).
func isCloudimgVM(vmID string) bool {
	_, err := os.Stat(vmRunPath(runDirCH, vmID, "overlay.qcow2"))
	return err == nil
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

// buildPostCloneCommands renders the in-guest fixup script. cloudimg → cloud-init;
// OCI → direct systemd-networkd writes.
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

	if isHTTPURL(sourceImage) || isCloudimgVM(vmID) {
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
