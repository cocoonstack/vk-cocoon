package cocoon

import (
	"fmt"
	"os"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"

	"github.com/cocoonstack/vk-cocoon/provider"
)

// CollectVMStats returns per-VM and node-level stats for the Prometheus
// collector. Called on every scrape from the metrics endpoint.
func (p *Provider) CollectVMStats() ([]provider.VMStats, provider.NodeStats) {
	snapshots := p.snapshotTrackedVMs()

	out := make([]provider.VMStats, 0, len(snapshots))
	for _, s := range snapshots {
		var rxBytes, txBytes uint64
		if s.Tap != "" {
			rxBytes, txBytes = readProcNetDev(s.PID, s.Tap)
		}
		out = append(out, provider.VMStats{
			VMName:     s.VMName,
			PodName:    s.PodName,
			Namespace:  s.Namespace,
			Backend:    s.Backend,
			CPUSeconds: readProcessCPUSeconds(s.PID),
			MemoryRSS:  readProcessMemoryWorkingSet(s.PID),
			DiskCOW:    readCOWSize(s.ID, s.Hypervisor),
			NetRxBytes: rxBytes,
			NetTxBytes: txBytes,
		})
	}

	node := provider.NodeStats{
		CPUSeconds:      readNodeCPUSeconds(),
		MemoryUsedBytes: readNodeMemoryWorkingSet(),
	}
	node.StorageTotal, node.StorageAvailable = provider.StorageBytes()

	return out, node
}

// readCOWSize returns the actual disk usage of a VM's writable overlay.
func readCOWSize(vmID, hypervisor string) int64 {
	rootDir := commonk8s.EnvOrDefault("COCOON_ROOT_DIR", "/var/lib/cocoon")
	dir := hypervisor
	if dir == "" {
		dir = "cloudhypervisor" // default backend
	}
	cowNames := cowFileNames(dir)
	for _, name := range cowNames {
		path := fmt.Sprintf("%s/run/%s/%s/%s", rootDir, dir, vmID, name)
		if fi, err := os.Stat(path); err == nil {
			return fi.Size()
		}
	}
	return 0
}

func cowFileNames(hypervisor string) []string {
	switch hypervisor {
	case "firecracker":
		return []string{"cow.raw"}
	default:
		return []string{"overlay.qcow2"}
	}
}
