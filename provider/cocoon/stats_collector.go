package cocoon

import (
	"fmt"
	"os"

	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/vm"
)

// runDir constants map VMM backends to their runtime directory names.
const (
	runDirCH = "cloudhypervisor"
	runDirFC = "firecracker"
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
	rootDir := provider.CocoonRootDir()
	dir := hypervisorRunDir(hypervisor)
	for _, name := range cowFileNames(dir) {
		path := fmt.Sprintf("%s/run/%s/%s/%s", rootDir, dir, vmID, name)
		if fi, err := os.Stat(path); err == nil {
			return fi.Size()
		}
	}
	return 0
}

// hypervisorRunDir maps the inspect "hypervisor" field to the actual
// run directory name (cocoon uses "cloudhypervisor" without a hyphen).
func hypervisorRunDir(hypervisor string) string {
	if hypervisor == vm.BackendFirecracker {
		return runDirFC
	}
	return runDirCH
}

func cowFileNames(dir string) []string {
	if dir == runDirFC {
		return []string{"cow.raw"}
	}
	return []string{"overlay.qcow2", "cow.raw"}
}
