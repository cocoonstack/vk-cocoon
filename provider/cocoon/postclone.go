package cocoon

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
	corev1 "k8s.io/api/core/v1"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/guest/sac"
	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const (
	// osWindows matches meta.VMSpec.OS for Windows pods.
	osWindows = "windows"

	// annotationPostCloneHint holds the base64-encoded fallback script
	// for manual recovery when runPostCloneSetup fails.
	annotationPostCloneHint = "vm.cocoonstack.io/post-clone-hint"

	// annotationPostCloneState observability for the auto-exec path:
	// running / done / failed.
	annotationPostCloneState = "vm.cocoonstack.io/post-clone-state"

	postCloneStateRunning = "running"
	postCloneStateDone    = "done"
	postCloneStateFailed  = "failed"

	// postCloneAgentBudget bounds the time spent retrying Runtime.Exec
	// while the cloned guest's cocoon-agent comes up. PnP-rebind on
	// Windows takes ~26s; Linux cloud-init can run minutes. 180s covers
	// the slow path.
	postCloneAgentBudget = 180 * time.Second

	// postCloneRetryInterval is the backoff between Runtime.Exec attempts.
	postCloneRetryInterval = 3 * time.Second
)

// runPostCloneSetup auto-executes the post-clone fixup script inside the
// cloned guest via cocoon-agent vsock, then writes the resulting state
// to vm.cocoonstack.io/post-clone-state. Falls back to writing the
// classic post-clone-hint annotation only on timeout/failure so the
// operator has a manual path.
func (p *Provider) runPostCloneSetup(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, v *vm.VM, sourceImage string) {
	plan, ok := planPostClone(spec, v, sourceImage)
	if !ok {
		return
	}
	logger := log.WithFunc("Provider.runPostCloneSetup")
	p.markPostCloneState(ctx, pod, postCloneStateRunning)

	// Bound the entire retry loop with a single context deadline. We
	// cannot rely on Runtime.Exec honoring the deadline directly:
	// Exec spawns `sudo cocoon vm exec ...`, and SIGKILL to sudo
	// doesn't propagate to the cocoon grandchild, so cmd.Wait can
	// block on the stdout pipe an orphaned cocoon process holds open.
	// Run each Exec in a worker goroutine and select on the deadline
	// instead so we abandon stuck workers rather than waiting on them.
	loopCtx, cancel := context.WithTimeout(ctx, postCloneAgentBudget)
	defer cancel()

	var lastErr error
	for {
		done := make(chan error, 1)
		go func() {
			done <- p.Runtime.Exec(loopCtx, v.ID, plan.argv, nil, nil, io.Discard, io.Discard)
		}()
		select {
		case execErr := <-done:
			if execErr == nil {
				logger.Infof(ctx, "post-clone setup succeeded for %s/%s", pod.Namespace, pod.Name)
				p.markPostCloneState(ctx, pod, postCloneStateDone)
				return
			}
			lastErr = execErr
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
	logger.Errorf(ctx, lastErr, "post-clone setup timed out for %s/%s after %s; falling back to manual hint",
		pod.Namespace, pod.Name, postCloneAgentBudget)
	p.markPostCloneState(ctx, pod, postCloneStateFailed)
	p.emitPostCloneHint(ctx, pod, spec, v, sourceImage)
}

// postClonePlan describes what to exec inside the guest after clone.
type postClonePlan struct {
	argv []string
}

// planPostClone returns the auto-exec plan for a cloned VM, or ok=false
// when no setup is needed (e.g. CH+OCI+DHCP self-heals via networkd
// hot-swap; cloudimg+DHCP self-heals via the netplan name-match fallback
// added in cocoonv2 be35341).
func planPostClone(spec meta.VMSpec, v *vm.VM, sourceImage string) (postClonePlan, bool) {
	if v == nil {
		return postClonePlan{}, false
	}
	if spec.OS == osWindows {
		// Chained Windows clones land with NDIS-stuck NICs (a4eeb0d).
		// PnP-rebind unsticks DHCP for free; static-IP NICs are still
		// configured by applyWindowsStaticIP via SAC in parallel.
		return postClonePlan{argv: buildWindowsPostCloneArgv()}, true
	}
	if !needsPostClone(spec.Backend, v.ID, sourceImage, v.NetworkConfigs) {
		return postClonePlan{}, false
	}
	script := buildPostCloneCommands(spec.VMName, spec.Backend, v.ID, sourceImage, v.NetworkConfigs)
	return postClonePlan{argv: []string{"sh", "-c", script}}, true
}

// markPostCloneState writes the state annotation and patches it back to
// the apiserver. Best-effort: a failed patch logs but does not block.
func (p *Provider) markPostCloneState(ctx context.Context, pod *corev1.Pod, state string) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[annotationPostCloneState] = state
	if err := p.patchPodAnnotations(ctx, pod.Namespace, pod.Name, map[string]any{annotationPostCloneState: state}); err != nil {
		log.WithFunc("Provider.markPostCloneState").
			Warnf(ctx, "patch post-clone state %s for %s/%s: %v", state, pod.Namespace, pod.Name, err)
	}
}

// emitPostCloneHint writes the manual-recovery script into a pod annotation.
// Called as fallback when runPostCloneSetup fails so an operator can
// reproduce the fix via cocoon vm console.
func (p *Provider) emitPostCloneHint(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, v *vm.VM, sourceImage string) {
	if spec.OS == osWindows {
		// Windows fallback: the same PnP-rebind PowerShell as the auto-exec path.
		ps := strings.Join(buildWindowsPostCloneArgv(), " ")
		writeHint(ctx, p, pod, ps)
		return
	}
	if !needsPostClone(spec.Backend, v.ID, sourceImage, v.NetworkConfigs) {
		return
	}
	writeHint(ctx, p, pod, buildPostCloneCommands(spec.VMName, spec.Backend, v.ID, sourceImage, v.NetworkConfigs))
}

func writeHint(ctx context.Context, p *Provider, pod *corev1.Pod, commands string) {
	logger := log.WithFunc("Provider.emitPostCloneHint")
	encoded := base64.StdEncoding.EncodeToString([]byte(commands))
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[annotationPostCloneHint] = encoded
	if err := p.patchPodAnnotations(ctx, pod.Namespace, pod.Name, map[string]any{annotationPostCloneHint: encoded}); err != nil {
		logger.Errorf(ctx, err, "patch post-clone hint %s/%s", pod.Namespace, pod.Name)
	}
	logger.Warnf(ctx, "manual post-clone setup required for %s/%s; hint at annotation %s",
		pod.Namespace, pod.Name, annotationPostCloneHint)
}

// needsPostClone reports whether a Linux cloned VM requires post-clone
// setup. After cocoonv2 be35341 (cloudimg netplan name-match fallback),
// cloudimg+DHCP self-heals like OCI+DHCP, so the only cloudimg case
// still needing intervention is static-IP — which is captured by the
// general isStaticNIC check below. FC clones always need MAC fixup.
//
// sourceImage is the snapshot's original image URL when available
// (normal clone path). When empty (forkFrom, wake), falls back to
// checking the COW file type on disk for buildPostCloneCommands' use.
func needsPostClone(backend, vmID, sourceImage string, networkConfigs []*vm.NetworkConfig) bool {
	_ = vmID
	_ = sourceImage
	if backend == vm.BackendFirecracker {
		return true
	}
	return slices.ContainsFunc(networkConfigs, isStaticNIC)
}

// buildWindowsPostCloneArgv returns the cocoon-agent argv that runs the
// PnP-rebind in the cloned Windows guest. Disable-PnpDevice followed by
// Enable-PnpDevice on every present Net device forces NDIS to rebind
// and clears the chained-clone DHCP-stuck state.
//
// -PresentOnly is load-bearing: chained-clone images accumulate ghost
// Net PnP entries from earlier MAC generations, and Disable-PnpDevice
// on a non-Present device returns HRESULT 0x80041001 and aborts the
// pipeline before reaching the real adapter.
func buildWindowsPostCloneArgv() []string {
	const ps = `Get-PnpDevice -Class Net -PresentOnly | ForEach-Object { ` +
		`Disable-PnpDevice -InstanceId $_.InstanceId -Confirm:$false; ` +
		`Start-Sleep 2; ` +
		`Enable-PnpDevice -InstanceId $_.InstanceId -Confirm:$false }`
	return []string{"powershell", "-NoProfile", "-Command", ps}
}

// isCloudimg checks whether the image string is a cloudimg URL.
func isCloudimg(image string) bool {
	return strings.HasPrefix(image, "http://") || strings.HasPrefix(image, "https://")
}

// isCloudimgVM checks whether the VM's writable disk is a qcow2 overlay
// (cloudimg) rather than a raw COW (OCI).
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

	// Open a persistent SAC session — all commands share one connection.
	sess, err := p.GuestSAC.Dial(ctx, sockPath)
	if err != nil {
		logger.Errorf(ctx, err, "sac dial %s/%s", pod.Namespace, pod.Name)
		return
	}
	defer func() { _ = sess.Close() }()

	// Query net numbers; retry until all NICs are enumerated.
	var out bytes.Buffer
	var netNums []int
	for attempt := range 60 {
		out.Reset()
		if queryErr := sess.Run(ctx, []string{"i"}, &out); queryErr != nil {
			logger.Debugf(ctx, "sac query: %v", queryErr)
		} else {
			netNums = sac.ParseNetEntries(out.String())
			if len(netNums) >= len(v.NetworkConfigs) {
				break
			}
		}
		if attempt == 59 {
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
		for attempt := range 10 {
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
			if attempt == 9 {
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
