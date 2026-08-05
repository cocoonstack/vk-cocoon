package cocoon

import (
	"github.com/cocoonstack/vk-cocoon/provider"
)

// CollectVMStats returns per-VM and node-level stats for the Prometheus
// collector. Called on every scrape from the metrics endpoint.
func (p *Provider) CollectVMStats() ([]provider.VMStats, provider.NodeStats) {
	samples, node := p.sampleStats()
	out := make([]provider.VMStats, 0, len(samples))
	for _, s := range samples {
		out = append(out, provider.VMStats{
			VMName:              s.VMName,
			PodName:             s.PodName,
			Namespace:           s.Namespace,
			Backend:             s.Backend,
			CPUSeconds:          s.cpuSeconds,
			CPUThrottledSeconds: s.throttledSeconds,
			CPUThrottledPeriods: s.nrThrottled,
			MemoryRSS:           s.memBytes,
			DiskCOW:             s.diskCOW,
			NetRxBytes:          s.rxBytes,
			NetTxBytes:          s.txBytes,
		})
	}
	return out, node
}
