package provider

import (
	"bufio"
	"cmp"
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

// discoverVM finds a VM by name using `cocoon inspect` and `cocoon list`.
func (p *CocoonProvider) discoverVM(ctx context.Context, name string) *CocoonVM {
	if p.discoverVMFn != nil {
		return p.discoverVMFn(ctx, name)
	}
	if out, err := p.cocoonExec(ctx, buildLegacyListArgs()...); err == nil {
		var vms []cocoonVMJSON
		if err := json.Unmarshal([]byte(out), &vms); err == nil {
			for _, v := range vms {
				if v.Name == name || v.Config.Name == name {
					return jsonToVM(v)
				}
			}
		}
	}
	if vm := p.inspectVM(ctx, name); vm != nil {
		return vm
	}
	out, err := p.cocoonExec(ctx, buildListArgs()...)
	if err != nil {
		return p.discoverVMText(ctx, name)
	}
	var vms []cocoonInspectJSON
	if err := json.Unmarshal([]byte(out), &vms); err != nil {
		return p.discoverVMText(ctx, name)
	}
	for _, v := range vms {
		if v.Name == name {
			return inspectToVM(v)
		}
	}
	return nil
}

// discoverVMByID finds a VM by ID, preferring JSON output when available.
func (p *CocoonProvider) discoverVMByID(ctx context.Context, vmID string) *CocoonVM {
	if p.discoverVMByIDFn != nil {
		return p.discoverVMByIDFn(ctx, vmID)
	}
	if out, err := p.cocoonExec(ctx, buildLegacyListArgs()...); err == nil {
		var vms []cocoonVMJSON
		if err := json.Unmarshal([]byte(out), &vms); err == nil {
			for _, v := range vms {
				if v.ID == vmID || strings.HasPrefix(v.ID, vmID) {
					return jsonToVM(v)
				}
			}
		}
	}
	if vm := p.inspectVM(ctx, vmID); vm != nil {
		return vm
	}
	if out, err := p.cocoonExec(ctx, buildListArgs()...); err == nil {
		var vms []cocoonInspectJSON
		if err := json.Unmarshal([]byte(out), &vms); err == nil {
			for _, v := range vms {
				if v.VMID == vmID || strings.HasPrefix(v.VMID, vmID) {
					return inspectToVM(v)
				}
			}
		}
	}
	out, _ := p.cocoonExec(ctx, buildLegacyTextListArgs()...)
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, vmID) || strings.HasPrefix(line, "VM ID") {
			continue
		}
		vm := parseTextVMLine(sc.Text())
		if vm == nil {
			continue
		}
		return vm
	}
	return nil
}

func discoverTextVMState(line string) string {
	line = strings.ToLower(line)
	switch {
	case strings.Contains(line, stateStopped) && strings.Contains(line, "stale"):
		return stateStoppedStale
	case strings.Contains(line, stateRunning):
		return stateRunning
	case strings.Contains(line, stateCreating):
		return stateCreating
	case strings.Contains(line, stateStopped):
		return stateStopped
	default:
		return stateUnknown
	}
}

func (p *CocoonProvider) discoverVMText(ctx context.Context, name string) *CocoonVM {
	out, _ := p.cocoonExec(ctx, buildLegacyTextListArgs()...)
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		vm := parseTextVMLine(sc.Text())
		if vm != nil && vm.vmName == name {
			vm.ip = cmp.Or(vm.ip, p.resolveIPFromLease(vm.vmName))
			return vm
		}
	}
	return nil
}

func jsonToVM(v cocoonVMJSON) *CocoonVM {
	cniIP := v.IP
	mac := ""
	if len(v.NetworkConfigs) > 0 {
		if v.NetworkConfigs[0].Network.IP != "" {
			cniIP = v.NetworkConfigs[0].Network.IP
		}
		mac = v.NetworkConfigs[0].MAC
	}
	if cniIP == "-" || cniIP == "" {
		cniIP = ""
	}

	ip := cniIP
	if mac != "" {
		if dhcpIP := resolveLeaseByMAC(mac, time.Time{}); dhcpIP != "" {
			ip = dhcpIP
		}
	}

	memMB := int(v.Memory / (1024 * 1024))
	if memMB == 0 && v.Config.Memory > 0 {
		memMB = int(v.Config.Memory / (1024 * 1024))
	}
	cpu := v.CPU
	if cpu == 0 {
		cpu = v.Config.CPU
	}
	vmName := v.Name
	if vmName == "" {
		vmName = v.Config.Name
	}
	return &CocoonVM{
		vmID:     v.ID,
		vmName:   vmName,
		state:    normalizedState(v.State),
		ip:       ip,
		mac:      mac,
		cpu:      cpu,
		memoryMB: memMB,
		image:    v.Image,
	}
}

func parseTextVMLine(line string) *CocoonVM {
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] == "VM" {
		return nil
	}
	cpu, _ := strconv.Atoi(fields[3])
	memMB := 0
	if trimmed, ok := strings.CutSuffix(fields[4], "MB"); ok {
		memMB, _ = strconv.Atoi(trimmed)
	}
	return &CocoonVM{
		vmID:     fields[0],
		vmName:   fields[1],
		state:    discoverTextVMState(line),
		cpu:      cpu,
		memoryMB: memMB,
	}
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
