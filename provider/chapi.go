// Package provider -- Cloud Hypervisor API client for VM metrics and control.
//
// Each Cloud Hypervisor process exposes a Unix socket API at:
//
//	/var/lib/cocoon/run/cloudhypervisor/{vmid}/api.sock
//	  (or /var/lib/cocoon/run/cloudhypervisor/{vmid}/api.sock)
//
// Endpoints used:
//
//	GET  /api/v1/vm.info      -- VM state + memory_actual_size
//	GET  /api/v1/vm.counters  -- Disk I/O + network stats per device
//	GET  /api/v1/vmm.ping     -- Health check + PID
//	PUT  /api/v1/vm.power-button -- ACPI power button (graceful shutdown)
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ---------- CH API response types ----------

// CHVMInfo is the response from GET /api/v1/vm.info (subset).
type CHVMInfo struct {
	State            string `json:"state"` // "Running", "Paused", "Created", etc.
	MemoryActualSize uint64 `json:"memory_actual_size"`
	Config           struct {
		CPUs struct {
			BootVCPUs int `json:"boot_vcpus"`
		} `json:"cpus"`
		Memory struct {
			Size uint64 `json:"size"`
		} `json:"memory"`
	} `json:"config"`
	DeviceTree map[string]json.RawMessage `json:"device_tree"`
}

// CHPing is the response from GET /api/v1/vmm.ping.
type CHPing struct {
	BuildVersion string `json:"build_version"`
	PID          int    `json:"pid"`
}

// CHCounters is the response from GET /api/v1/vm.counters.
type CHCounters map[string]map[string]uint64

// ---------- Socket discovery ----------

// chSocketPath returns the CH API socket path for a given VM ID.
// Checks both /var/lib/cocoon/run and /var/lib/cocoon/run.
func chSocketPath(vmID string) string {
	for _, base := range []string{
		"/var/lib/cocoon/run/cloudhypervisor",
		"/var/lib/cocoon/run/cloudhypervisor",
	} {
		sock := filepath.Join(base, vmID, "api.sock")
		if fi, err := os.Stat(sock); err == nil && fi.Mode().Type()&os.ModeSocket != 0 {
			return sock
		}
	}
	return ""
}

// ---------- Low-level HTTP helpers ----------

// chGet calls a CH API GET endpoint via sudo curl (socket is root-owned).
func chGet(ctx context.Context, socketPath, endpoint string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", "curl", "-s", "--unix-socket", socketPath, //nolint:gosec // trusted internal args
		"http://localhost"+endpoint)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ch api %s: %w", endpoint, err)
	}
	return out, nil
}

// ---------- CH API calls ----------

// chGetPing calls GET /api/v1/vmm.ping and returns PID.
func chGetPing(ctx context.Context, vmID string) (*CHPing, error) {
	sock := chSocketPath(vmID)
	if sock == "" {
		return nil, fmt.Errorf("no CH socket for VM %s", vmID)
	}
	data, err := chGet(ctx, sock, "/api/v1/vmm.ping")
	if err != nil {
		return nil, err
	}
	var ping CHPing
	if err := json.Unmarshal(data, &ping); err != nil {
		return nil, fmt.Errorf("vmm.ping decode: %w", err)
	}
	return &ping, nil
}

// chGetCounters calls GET /api/v1/vm.counters for per-device I/O stats.
func chGetCounters(ctx context.Context, vmID string) (CHCounters, error) {
	sock := chSocketPath(vmID)
	if sock == "" {
		return nil, fmt.Errorf("no CH socket for VM %s", vmID)
	}
	data, err := chGet(ctx, sock, "/api/v1/vm.counters")
	if err != nil {
		return nil, err
	}
	var counters CHCounters
	if err := json.Unmarshal(data, &counters); err != nil {
		return nil, fmt.Errorf("vm.counters decode: %w", err)
	}
	return counters, nil
}

// ---------- Host /proc metrics ----------

// hostCPUCount caches the logical CPU count (never changes at runtime).
var hostCPUCount = func() int {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	count := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "processor") {
			count++
		}
	}
	return count
}()

// readHostCPUCount returns the number of logical CPUs on the host.
func readHostCPUCount() int {
	return hostCPUCount
}

// readMeminfoField reads a single field from /proc/meminfo, returning the
// value in bytes.  Returns 0 if the field is not found or the file cannot
// be read.
func readMeminfoField(field string) uint64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	prefix := field + ":"
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseUint(fields[1], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}

// readHostMemoryBytes returns total physical memory in bytes.
func readHostMemoryBytes() uint64 {
	return readMeminfoField("MemTotal")
}

// readHostMemAvailable returns available memory in bytes from /proc/meminfo.
func readHostMemAvailable() uint64 {
	return readMeminfoField("MemAvailable")
}

// readHostDiskBytes returns total and available bytes for a path via statfs.
func readHostDiskBytes(path string) (total, avail uint64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	total = stat.Blocks * uint64(stat.Bsize) //nolint:gosec // Bsize is always positive on Linux
	avail = stat.Bavail * uint64(stat.Bsize) //nolint:gosec // Bsize is always positive on Linux
	return total, avail
}

// ---------- Per-process /proc metrics ----------

// readProcCPUUsage reads CPU time (in nanoseconds) for a process from /proc/{pid}/stat.
// Returns (user_ns, system_ns, err).
func readProcCPUUsage(pid int) (uint64, uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	// /proc/pid/stat format: pid (comm) state ppid ... utime stime ...
	// Fields are space-separated, but comm can contain spaces/parens.
	// Find the last ')' to skip comm field.
	s := string(data)
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return 0, 0, fmt.Errorf("invalid /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[idx+1:])
	// fields[0] = state, fields[1] = ppid, ..., fields[11] = utime, fields[12] = stime
	if len(fields) < 13 {
		return 0, 0, fmt.Errorf("/proc/%d/stat: not enough fields", pid)
	}
	utime, _ := strconv.ParseUint(fields[11], 10, 64)
	stime, _ := strconv.ParseUint(fields[12], 10, 64)
	// Convert from clock ticks to nanoseconds (assume 100 Hz = 10ms per tick)
	const tickNs = 10_000_000 // 10ms in nanoseconds
	return utime * tickNs, stime * tickNs, nil
}

// readProcMemoryRSS reads VmRSS (resident memory) for a process in bytes.
func readProcMemoryRSS(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseUint(fields[1], 10, 64)
				return kb * 1024, nil
			}
		}
	}
	return 0, fmt.Errorf("vmrss not found for pid %d", pid)
}
