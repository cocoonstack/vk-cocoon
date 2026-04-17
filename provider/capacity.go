package provider

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
)

const (
	defaultReservePercent = 20
	defaultMaxPods        = 256
)

// NodeResources probes the host for real CPU, memory, hugepages, and
// storage, then returns Capacity (full host) and Allocatable (capacity
// minus the reserved fraction). The reserve percentage defaults to 20%
// and is overridable via VK_RESERVE_PERCENT. Individual resource values
// can still be force-overridden via VK_NODE_CPU / VK_NODE_MEM /
// VK_NODE_STORAGE / VK_NODE_HUGEPAGES / VK_NODE_PODS.
func NodeResources() (capacity, allocatable corev1.ResourceList, err error) {
	reservePct := defaultReservePercent
	if v := os.Getenv("VK_RESERVE_PERCENT"); v != "" {
		n, parseErr := strconv.Atoi(v)
		if parseErr != nil || n < 0 || n > 100 {
			return nil, nil, fmt.Errorf("parse VK_RESERVE_PERCENT=%q: must be 0-100", v)
		}
		reservePct = n
	}

	cpu, err := detectOrOverride("VK_NODE_CPU", detectCPU)
	if err != nil {
		return nil, nil, fmt.Errorf("detect cpu: %w", err)
	}
	mem, err := detectOrOverride("VK_NODE_MEM", detectMemory)
	if err != nil {
		return nil, nil, fmt.Errorf("detect memory: %w", err)
	}
	storageTotal, storageAvail, err := detectStorageOrOverride()
	if err != nil {
		return nil, nil, fmt.Errorf("detect storage: %w", err)
	}
	hugepages, err := detectOrOverride("VK_NODE_HUGEPAGES", detectHugepages)
	if err != nil {
		return nil, nil, fmt.Errorf("detect hugepages: %w", err)
	}
	pods, err := parseQuantityEnv("VK_NODE_PODS", strconv.Itoa(defaultMaxPods))
	if err != nil {
		return nil, nil, err
	}

	capacity = corev1.ResourceList{
		corev1.ResourceCPU:              cpu,
		corev1.ResourceMemory:           mem,
		corev1.ResourceEphemeralStorage: storageTotal,
		corev1.ResourcePods:             pods,
	}
	if !hugepages.IsZero() {
		capacity[corev1.ResourceHugePagesPrefix+"2Mi"] = hugepages
	}

	allocatable = make(corev1.ResourceList, len(capacity))
	for k, v := range capacity {
		if k == corev1.ResourcePods {
			allocatable[k] = v
			continue
		}
		allocatable[k] = reserveQuantity(v, reservePct)
	}
	// Storage allocatable is based on fs-available (excludes base images
	// and other existing data), not fs-total.
	allocatable[corev1.ResourceEphemeralStorage] = reserveQuantity(storageAvail, reservePct)
	return capacity, allocatable, nil
}

// reserveQuantity returns q * (100 - pct) / 100, rounding down.
func reserveQuantity(q resource.Quantity, pct int) resource.Quantity {
	v := q.Value()
	alloc := v * int64(100-pct) / 100
	return *resource.NewQuantity(alloc, q.Format)
}

// detectOrOverride returns the env-var override if set, otherwise calls
// the detect function to probe the host.
func detectOrOverride(envKey string, detect func() (resource.Quantity, error)) (resource.Quantity, error) {
	if v := os.Getenv(envKey); v != "" {
		q, err := resource.ParseQuantity(v)
		if err != nil {
			return resource.Quantity{}, fmt.Errorf("parse %s=%q: %w", envKey, v, err)
		}
		return q, nil
	}
	return detect()
}

func detectCPU() (resource.Quantity, error) {
	n := runtime.NumCPU()
	if n <= 0 {
		return resource.Quantity{}, fmt.Errorf("runtime.NumCPU returned %d", n)
	}
	return *resource.NewQuantity(int64(n), resource.DecimalSI), nil
}

func detectMemory() (resource.Quantity, error) {
	fields, err := readProcMemInfoFields("MemTotal")
	if err != nil {
		return resource.Quantity{}, err
	}
	return *resource.NewQuantity(fields["MemTotal"]*1024, resource.BinarySI), nil
}

func detectHugepages() (resource.Quantity, error) {
	fields, err := readProcMemInfoFields("HugePages_Total", "Hugepagesize")
	if err != nil {
		return resource.Quantity{}, nil //nolint:nilerr // missing fields = no hugepages
	}
	total := fields["HugePages_Total"]
	pageSizeKB := fields["Hugepagesize"]
	if total == 0 || pageSizeKB == 0 {
		return resource.Quantity{}, nil
	}
	return *resource.NewQuantity(total*pageSizeKB*1024, resource.BinarySI), nil
}

// detectStorageOrOverride returns filesystem total and available bytes.
// When VK_NODE_STORAGE is set, both total and available use that value
// (manual override disables the available-based allocatable logic).
func detectStorageOrOverride() (total, avail resource.Quantity, err error) {
	if v := os.Getenv("VK_NODE_STORAGE"); v != "" {
		q, parseErr := resource.ParseQuantity(v)
		if parseErr != nil {
			return resource.Quantity{}, resource.Quantity{}, fmt.Errorf("parse VK_NODE_STORAGE=%q: %w", v, parseErr)
		}
		return q, q, nil
	}
	rootDir := commonk8s.EnvOrDefault("COCOON_ROOT_DIR", "/var/lib/cocoon")
	var stat syscallStatfs
	if err := statfs(rootDir, &stat); err != nil {
		return resource.Quantity{}, resource.Quantity{}, fmt.Errorf("statfs %s: %w", rootDir, err)
	}
	totalQ := *resource.NewQuantity(statTotalBytes(stat), resource.BinarySI)
	availQ := *resource.NewQuantity(statAvailBytes(stat), resource.BinarySI)
	return totalQ, availQ, nil
}

// readProcMemInfoFields reads the named fields from /proc/meminfo in a
// single pass. Values are returned in kB (the unit /proc/meminfo uses).
func readProcMemInfoFields(names ...string) (map[string]int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, fmt.Errorf("open /proc/meminfo: %w", err)
	}
	defer f.Close() //nolint:errcheck

	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	result := make(map[string]int64, len(names))
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) < 2 {
			continue
		}
		key := strings.TrimSuffix(parts[0], ":")
		if !want[key] {
			continue
		}
		v, parseErr := strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("/proc/meminfo: parse %s: %w", key, parseErr)
		}
		result[key] = v
		if len(result) == len(want) {
			break // found all requested fields
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	if len(result) != len(want) {
		for _, n := range names {
			if _, ok := result[n]; !ok {
				return nil, fmt.Errorf("/proc/meminfo: %s not found", n)
			}
		}
	}
	return result, nil
}

func parseQuantityEnv(key, fallback string) (resource.Quantity, error) {
	raw := commonk8s.EnvOrDefault(key, fallback)
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("parse %s=%q: %w", key, raw, err)
	}
	return q, nil
}
