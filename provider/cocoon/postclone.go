package cocoon

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
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
	// annotationPostCloneHint holds base64-encoded shell commands the user
	// must run inside the guest to fix networking after clone.
	annotationPostCloneHint = "vm.cocoonstack.io/post-clone-hint"
)

// emitPostCloneHint checks whether the cloned VM needs manual network
// setup and, if so, writes the required commands into a pod annotation
// and logs a warning. The probe loop will flip the pod to Ready once
// the user executes the commands and network becomes reachable.
// sourceImage is the snapshot's original image (e.g. cloudimg URL).
// Empty when the source metadata is unavailable (forkFrom, wake).
func (p *Provider) emitPostCloneHint(ctx context.Context, pod *corev1.Pod, spec meta.VMSpec, v *vm.VM, sourceImage string) {
	// Windows networking is handled by SAC (applyWindowsStaticIP) or
	// auto-DHCP; the Linux shell/cloud-init hint would be misleading.
	if spec.OS == "windows" {
		return
	}
	if !needsPostClone(spec.Backend, v.ID, sourceImage, v.NetworkConfigs) {
		return
	}
	logger := log.WithFunc("Provider.emitPostCloneHint")
	commands := buildPostCloneCommands(spec.VMName, spec.Backend, v.ID, sourceImage, v.NetworkConfigs)
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(commands))
	pod.Annotations[annotationPostCloneHint] = encoded

	if err := p.patchPodAnnotations(ctx, pod.Namespace, pod.Name, map[string]any{annotationPostCloneHint: encoded}); err != nil {
		logger.Errorf(ctx, err, "patch post-clone hint %s/%s", pod.Namespace, pod.Name)
	}

	logger.Warnf(ctx, "VM %s requires manual network setup via cocoon vm console; hint written to annotation %s",
		spec.VMName, annotationPostCloneHint)
}

// needsPostClone reports whether a cloned VM requires manual network
// setup via console. The only automatic case is CH + OCI + all-DHCP
// (NIC hot-swap triggers networkd to re-DHCP). Everything else needs
// intervention: FC needs MAC fixup, cloudimg needs cloud-init re-run
// (snapshot restore does not re-trigger cloud-init), and static IP
// needs networkd config rewrite.
//
// sourceImage is the snapshot's original image URL when available
// (normal clone path). When empty (forkFrom, wake), falls back to
// checking the COW file type on disk.
func needsPostClone(backend, vmID, sourceImage string, networkConfigs []*vm.NetworkConfig) bool {
	if backend == vm.BackendFirecracker {
		return true
	}
	if isCloudimg(sourceImage) || isCloudimgVM(vmID) {
		return true // cloudimg: cloud-init must be re-triggered manually
	}
	// CH + OCI: only needs intervention if any NIC has a static IP
	return slices.ContainsFunc(networkConfigs, isStaticNIC)
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
				i, i, nc.MAC, i))
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
