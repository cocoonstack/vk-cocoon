package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/projecteru2/core/log"
)

func normalizedState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func (p *CocoonProvider) getVM(ns, name string) *CocoonVM {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.vms[podKey(ns, name)]
}

func (p *CocoonProvider) sshPass(vm *CocoonVM) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if pod, ok := p.pods[podKey(vm.podNamespace, vm.podName)]; ok {
		if pw := ann(pod, AnnSSHPassword, ""); pw != "" {
			return pw
		}
	}
	return p.sshPassword
}

var cocoonPassThroughEnv = []string{
	"COCOON_CONFIG_PATH",
	"COCOON_ROOT_DIR",
	"COCOON_RUNTIME_DIR",
	"COCOON_RUN_DIR",
	"COCOON_LOG_DIR",
	"COCOON_CNI_CONF_DIR",
	"COCOON_CNI_BIN_DIR",
	"COCOON_DNS",
	"COCOON_BALLOON_ENABLED",
	"COCOON_ROOT_PASSWORD",
	"COCOON_CH_BINARY",
	"COCOON_WINDOWS_CH_BINARY",
}

func (p *CocoonProvider) cocoonExec(ctx context.Context, args ...string) (string, error) {
	if p.cocoonExecFn != nil {
		return p.cocoonExecFn(ctx, args...)
	}
	sudoArgs := make([]string, 0, len(args)+len(cocoonPassThroughEnv)+2)
	var envArgs []string
	for _, key := range cocoonPassThroughEnv {
		if val := strings.TrimSpace(os.Getenv(key)); val != "" {
			envArgs = append(envArgs, key+"="+val)
		}
	}
	if len(envArgs) > 0 {
		sudoArgs = append(sudoArgs, "env")
		sudoArgs = append(sudoArgs, envArgs...)
	}
	sudoArgs = append(sudoArgs, p.cocoonBin)
	sudoArgs = append(sudoArgs, args...)
	cmd := exec.CommandContext(ctx, "sudo", sudoArgs...) //nolint:gosec // trusted cocoon CLI args
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (p *CocoonProvider) removeVM(ctx context.Context, ref string) {
	if strings.TrimSpace(ref) == "" {
		return
	}
	if _, err := p.cocoonExec(ctx, buildLegacyDeleteArgs(ref)...); err == nil {
		return
	}
	_, _ = p.cocoonExec(ctx, buildDeleteArgs(ref)...)
}

func (p *CocoonProvider) inspectVM(ctx context.Context, ref string) *CocoonVM {
	out, err := p.cocoonExec(ctx, buildInspectArgs(ref)...)
	if err != nil {
		return nil
	}
	var inspect cocoonInspectJSON
	if err := json.Unmarshal([]byte(out), &inspect); err != nil {
		return nil
	}
	return inspectToVM(inspect)
}

// discoverVM finds a VM by name, checking the event stream cache first,
// then falling back to a single cocoon inspect call.
func (p *CocoonProvider) discoverVM(ctx context.Context, name string) *CocoonVM {
	if p.discoverVMFn != nil {
		return p.discoverVMFn(ctx, name)
	}
	if p.vmState != nil {
		if cv := p.vmState.findByName(name); cv != nil {
			return cv.toCocoonVM()
		}
	}
	return p.inspectVM(ctx, name)
}

// discoverVMByID finds a VM by ID, checking the event stream cache first,
// then falling back to a single cocoon inspect call.
func (p *CocoonProvider) discoverVMByID(ctx context.Context, vmID string) *CocoonVM {
	if p.discoverVMByIDFn != nil {
		return p.discoverVMByIDFn(ctx, vmID)
	}
	if p.vmState != nil {
		if cv := p.vmState.findByID(vmID); cv != nil {
			return cv.toCocoonVM()
		}
	}
	return p.inspectVM(ctx, vmID)
}

func inspectToVM(v cocoonInspectJSON) *CocoonVM {
	var createdAt time.Time
	if ts := strings.TrimSpace(v.Timestamps.CreatedAt); ts != "" {
		createdAt, _ = time.Parse(time.RFC3339, ts)
	}
	var startedAt time.Time
	if ts := strings.TrimSpace(v.Timestamps.StartedAt); ts != "" {
		startedAt, _ = time.Parse(time.RFC3339, ts)
	}
	return &CocoonVM{
		vmID:      v.VMID,
		vmName:    v.Name,
		state:     normalizedState(v.State),
		ip:        resolveLeaseByIdentity(v.Name, time.Time{}),
		cpu:       v.BootConfig.CPUs,
		memoryMB:  int(v.BootConfig.MemoryMB),
		image:     v.Image.Ref,
		createdAt: createdAt,
		startedAt: startedAt,
	}
}

// resolveIPFromLeaseByMAC is a standalone helper (no freshness check, for reconcile).
func resolveIPFromLeaseByMAC(mac string) string {
	return resolveLeaseByMAC(mac, time.Time{})
}

// waitForDHCPIP polls dnsmasq leases until a DHCP IP appears for the VM.
func (p *CocoonProvider) waitForDHCPIP(ctx context.Context, vm *CocoonVM, timeout time.Duration) string {
	if p.waitForDHCPIPFn != nil {
		return p.waitForDHCPIPFn(ctx, vm, timeout)
	}
	deadline := time.Now().Add(timeout)
	notBefore := time.Now().Add(-60 * time.Second)
	logger := log.WithFunc("provider.waitForDHCPIP")
	logger.Infof(ctx, "VM %s mac=%s, polling leases (timeout %s)", vm.vmName, vm.mac, timeout)
	for time.Now().Before(deadline) {
		if vm.mac != "" {
			if ip := resolveLeaseByMAC(vm.mac, notBefore); ip != "" {
				logger.Infof(ctx, "VM %s got DHCP IP %s (by MAC)", vm.vmName, ip)
				return ip
			}
		}
		if vm.vmName != "" {
			if ip := resolveLeaseByIdentity(vm.vmName, notBefore); ip != "" {
				logger.Infof(ctx, "VM %s got DHCP IP %s (by hostname)", vm.vmName, ip)
				return ip
			}
		}
		time.Sleep(2 * time.Second)
	}
	logger.Warnf(ctx, "VM %s DHCP timeout, falling back to %s", vm.vmName, vm.ip)
	return vm.ip
}

// resolveLeaseByIdentity finds the DHCP IP by hostname or MAC address.
func resolveLeaseByIdentity(identity string, notBefore time.Time) string {
	data, _ := os.ReadFile("/var/lib/misc/dnsmasq.leases")
	minTS := notBefore.Unix()
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 4 && (fields[1] == identity || fields[3] == identity) {
			if minTS > 0 {
				ts, _ := strconv.ParseInt(fields[0], 10, 64)
				if ts < minTS {
					continue
				}
			}
			return fields[2]
		}
	}
	return ""
}

// resolveLeaseByMAC finds the DHCP IP for a MAC address.
func resolveLeaseByMAC(mac string, notBefore time.Time) string {
	return resolveLeaseByIdentity(mac, notBefore)
}

// resolveIPFromLease reads dnsmasq leases to find IP by hostname or MAC.
func (p *CocoonProvider) resolveIPFromLease(hostnameOrMAC string) string {
	return resolveLeaseByIdentity(hostnameOrMAC, time.Time{})
}
