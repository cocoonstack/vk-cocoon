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
	"github.com/cocoonstack/vk-cocoon/guest/sac"
	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	annotationPostCloneHint  = "vm.cocoonstack.io/post-clone-hint"
	annotationPostCloneState = "vm.cocoonstack.io/post-clone-state"

	postCloneStateRunning = "running"
	postCloneStateDone    = "done"
	postCloneStateFailed  = "failed"

	postCloneAgentBudget   = 180 * time.Second
	postCloneRetryInterval = 3 * time.Second

	sacEnumRetries  = 60 // waits for Windows PnP to surface every NIC
	sacIPSetRetries = 10 // per-NIC retry verifying `i <n>...` took
)

// runPostCloneSetup auto-executes the post-clone fixup inside the cloned
// guest via cocoon-agent vsock and records the outcome in
// vm.cocoonstack.io/post-clone-state. On timeout or failure it falls back
// to writing the post-clone-hint annotation so an operator has a manual path.
func (p *Provider) runPostCloneSetup(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, v *vm.VM, sourceImage string) {
	plan, ok := planPostClone(spec, v, sourceImage)
	if !ok {
		// No fixup required — DHCP self-heals on CH+OCI/cloudimg.
		p.markLifecycleState(ctx, pod, meta.LifecycleStateReady, "")
		return
	}
	logger := log.WithFunc("Provider.runPostCloneSetup")
	t0 := time.Now()
	logger.Infof(ctx, "post-clone setup starting for %s/%s vm=%s os=%q backend=%q",
		pod.Namespace, pod.Name, v.ID, spec.OS, spec.Backend)
	p.markPostCloneState(ctx, pod, postCloneStateRunning)

	// SIGKILL to the sudo wrapper from exec.CommandContext doesn't
	// propagate to the grandchild cocoon process, so cmd.Wait can block
	// on its open stdout pipe. Drive each Exec in a worker and select
	// on the deadline so we abandon stuck workers instead of inheriting
	// their hang.
	loopCtx, cancel := context.WithTimeout(ctx, postCloneAgentBudget)
	defer cancel()

	var lastErr error
	for attempt := 1; ; attempt++ {
		attemptStart := time.Now()
		done := make(chan error, 1)
		go func() {
			done <- p.Runtime.Exec(loopCtx, v.ID, plan.argv, nil, nil, io.Discard, io.Discard)
		}()
		select {
		case execErr := <-done:
			if execErr == nil {
				logger.Infof(ctx, "post-clone setup succeeded for %s/%s vm=%s attempts=%d attempt_dur=%s total_dur=%s",
					pod.Namespace, pod.Name, v.ID, attempt, time.Since(attemptStart).Round(time.Millisecond), time.Since(t0).Round(time.Millisecond))
				p.markPostCloneState(ctx, pod, postCloneStateDone)
				p.markLifecycleState(ctx, pod, meta.LifecycleStateReady, "")
				return
			}
			lastErr = execErr
			logger.Warnf(ctx, "post-clone exec attempt %d failed for %s/%s after %s: %v",
				attempt, pod.Namespace, pod.Name, time.Since(attemptStart).Round(time.Millisecond), execErr)
		case <-loopCtx.Done():
			lastErr = loopCtx.Err()
		}
		if loopCtx.Err() != nil {
			break
		}
		if !commonk8s.SleepCtx(loopCtx, postCloneRetryInterval) {
			break
		}
	}
	if errors.Is(lastErr, context.Canceled) {
		// Provider shutdown canceled lifecycleCtx; the pod will be re-
		// reconciled on next start. Skip the failed-state + hint writes
		// because their patch ctx is the same canceled one.
		logger.Infof(ctx, "post-clone setup canceled for %s/%s after %s (provider shutdown)",
			pod.Namespace, pod.Name, time.Since(t0).Round(time.Millisecond))
		return
	}
	logger.Errorf(ctx, lastErr, "post-clone setup timed out for %s/%s after %s (budget %s); falling back to manual hint",
		pod.Namespace, pod.Name, time.Since(t0).Round(time.Millisecond), postCloneAgentBudget)
	p.markPostCloneState(ctx, pod, postCloneStateFailed)
	p.markLifecycleState(ctx, pod, meta.LifecycleStateFailed, lastErr.Error())
	p.emitPostCloneHint(ctx, pod, spec, v, sourceImage)
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
	p.setPodAnnotation(ctx, pod, annotationPostCloneState, state)
}

// emitPostCloneHint records a manual-recovery script in pod annotations
// when runPostCloneSetup gives up. Operators decode and run it from a
// cocoon vm console session.
func (p *Provider) emitPostCloneHint(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, v *vm.VM, sourceImage string) {
	plan, ok := planPostClone(spec, v, sourceImage)
	if !ok {
		return
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(plan.hint))
	p.setPodAnnotation(ctx, pod, annotationPostCloneHint, encoded)
	log.WithFunc("Provider.emitPostCloneHint").Warnf(ctx,
		"manual post-clone setup required for %s/%s; hint at annotation %s",
		pod.Namespace, pod.Name, annotationPostCloneHint)
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
// Called for both run and clone when the network uses IPAM.
// DHCP NICs (no assigned IP) are skipped.
func (p *Provider) applyWindowsStaticIP(ctx context.Context, pod *corev1.Pod, v *vm.VM) {
	if p.GuestSAC == nil || len(v.NetworkConfigs) == 0 {
		return
	}
	if !slices.ContainsFunc(v.NetworkConfigs, isStaticNIC) {
		return
	}

	logger := log.WithFunc("Provider.applyWindowsStaticIP")
	sockPath := fmt.Sprintf("%s/run/%s/%s/console.sock", provider.CocoonRootDir(), runDirCH, v.ID)

	sess, err := p.GuestSAC.Dial(ctx, sockPath)
	if err != nil {
		logger.Errorf(ctx, err, "sac dial %s/%s", pod.Namespace, pod.Name)
		return
	}
	defer func() { _ = sess.Close() }()

	// Query net numbers; retry until all NICs are enumerated.
	var out bytes.Buffer
	var netNums []int
	for attempt := range sacEnumRetries {
		out.Reset()
		if queryErr := sess.Run(ctx, []string{"i"}, &out); queryErr != nil {
			logger.Debugf(ctx, "sac query: %v", queryErr)
		} else {
			netNums = sac.ParseNetEntries(out.String())
			if len(netNums) >= len(v.NetworkConfigs) {
				break
			}
		}
		if attempt == sacEnumRetries-1 {
			logger.Warnf(ctx, "sac: found %d net entries but need %d for %s/%s after retries",
				len(netNums), len(v.NetworkConfigs), pod.Namespace, pod.Name)
			return
		}
		if !commonk8s.SleepCtx(ctx, 2*time.Second) {
			return
		}
	}

	// Set IP on each static NIC with verify+retry.
	for i, nc := range v.NetworkConfigs {
		if !isStaticNIC(nc) {
			continue
		}
		cmd := []string{
			"i", strconv.Itoa(netNums[i]),
			nc.Network.IP, prefixToSubnet(nc.Network.Prefix), nc.Network.Gateway,
		}
		for attempt := range sacIPSetRetries {
			if setErr := sess.Run(ctx, cmd, nil); setErr != nil {
				logger.Errorf(ctx, setErr, "sac set ip net %d for %s/%s", netNums[i], pod.Namespace, pod.Name)
				return
			}
			out.Reset()
			if verifyErr := sess.Run(ctx, []string{"i"}, &out); verifyErr != nil {
				logger.Debugf(ctx, "sac verify: %v", verifyErr)
			} else if sac.NetHasIP(out.String(), netNums[i], nc.Network.IP) {
				break
			}
			if attempt == sacIPSetRetries-1 {
				logger.Warnf(ctx, "sac: net %d did not accept ip %s after retries for %s/%s",
					netNums[i], nc.Network.IP, pod.Namespace, pod.Name)
				return
			}
			logger.Debugf(ctx, "sac: net %d ip not yet effective, retrying in 2s", netNums[i])
			if !commonk8s.SleepCtx(ctx, 2*time.Second) {
				return
			}
		}
	}
	logger.Infof(ctx, "sac configured static IPs for %s/%s", pod.Namespace, pod.Name)
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
