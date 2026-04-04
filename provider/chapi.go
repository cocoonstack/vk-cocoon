package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// CHVMInfo is the response from GET /api/v1/vm.info (subset).
type CHVMInfo struct {
	State            string `json:"state"`
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

type chCacheEntry struct {
	data      []byte
	fetchedAt time.Time
}

var (
	chCacheMu  sync.RWMutex
	chCache    = map[string]*chCacheEntry{}
	chCacheTTL = 8 * time.Second
)

func chSocketPath(vmID string) string {
	base := "/run/cocoon/" + vmID + "/ch.sock"
	if fi, err := os.Stat(base); err == nil && fi.Mode().Type()&os.ModeSocket != 0 {
		return base
	}
	for _, dir := range []string{
		"/var/lib/cocoon/run/cloudhypervisor",
	} {
		sock := filepath.Join(dir, vmID, "api.sock")
		if fi, err := os.Stat(sock); err == nil && fi.Mode().Type()&os.ModeSocket != 0 {
			return sock
		}
	}
	return ""
}

func chHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", socketPath, 3*time.Second)
			},
		},
	}
}

func chGet(ctx context.Context, socketPath, endpoint string) ([]byte, error) {
	key := socketPath + endpoint
	chCacheMu.RLock()
	if e, ok := chCache[key]; ok && time.Since(e.fetchedAt) < chCacheTTL {
		chCacheMu.RUnlock()
		return e.data, nil
	}
	chCacheMu.RUnlock()

	client := chHTTPClient(socketPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("ch api %s: %w", endpoint, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ch api %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ch api %s read: %w", endpoint, err)
	}

	chCacheMu.Lock()
	chCache[key] = &chCacheEntry{data: data, fetchedAt: time.Now()}
	chCacheMu.Unlock()

	return data, nil
}

func chGetPing(ctx context.Context, vmID string) (*CHPing, error) {
	sock := chSocketPath(vmID)
	if sock == "" {
		return nil, fmt.Errorf("no ch socket for vm %s", vmID)
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

func chGetCounters(ctx context.Context, vmID string) (CHCounters, error) {
	sock := chSocketPath(vmID)
	if sock == "" {
		return nil, fmt.Errorf("no ch socket for vm %s", vmID)
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

func readHostCPUCount() int {
	return hostCPUCount
}

func readMeminfo(fields ...string) map[string]uint64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil
	}
	result := make(map[string]uint64, len(fields))
	for line := range strings.SplitSeq(string(data), "\n") {
		for _, field := range fields {
			prefix := field + ":"
			if strings.HasPrefix(line, prefix) {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					kb, _ := strconv.ParseUint(parts[1], 10, 64)
					result[field] = kb * 1024
				}
			}
		}
	}
	return result
}

func readHostMemoryBytes() uint64 {
	return readMeminfo("MemTotal")["MemTotal"]
}

func readHostDiskBytes(path string) (total, avail uint64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	total = stat.Blocks * uint64(stat.Bsize) //nolint:gosec // Bsize is always positive on Linux
	avail = stat.Bavail * uint64(stat.Bsize) //nolint:gosec // Bsize is always positive on Linux
	return total, avail
}

func readProcCPUUsage(pid int) (uint64, uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	s := string(data)
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return 0, 0, fmt.Errorf("invalid /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[idx+1:])
	if len(fields) < 13 {
		return 0, 0, fmt.Errorf("/proc/%d/stat: not enough fields", pid)
	}
	utime, _ := strconv.ParseUint(fields[11], 10, 64)
	stime, _ := strconv.ParseUint(fields[12], 10, 64)
	const tickNs = 10_000_000
	return utime * tickNs, stime * tickNs, nil
}

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
