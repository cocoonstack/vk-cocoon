package vm

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultCgroupParent mirrors cocoon's cgroup_parent default.
const DefaultCgroupParent = "cocoon.slice"

// ScopeCPUStat reads a VM's cpu.stat under parent; zeros when the scope is gone or the parent mismatches.
func ScopeCPUStat(parent, vmID string) (usageSeconds, throttledSeconds float64, nrThrottled int64) {
	data, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", parent, "vm-"+vmID+".scope", "cpu.stat")) //nolint:gosec // path derives from operator config + tracked VM ID
	if err != nil {
		return 0, 0, 0
	}
	usageUs, throttledUs, throttled := parseCPUStat(string(data))
	return float64(usageUs) / 1e6, float64(throttledUs) / 1e6, throttled
}

func parseCPUStat(s string) (usageUs, throttledUs, nrThrottled int64) {
	for line := range strings.Lines(s) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "usage_usec":
			usageUs = n
		case "throttled_usec":
			throttledUs = n
		case "nr_throttled":
			nrThrottled = n
		}
	}
	return usageUs, throttledUs, nrThrottled
}
