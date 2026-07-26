package cocoon

import (
	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/vm"
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
			DiskCOW:    vm.COWSize(provider.CocoonRootDir(), s.Hypervisor, s.ID),
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
