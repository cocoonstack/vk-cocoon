package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/cocoonstack/vk-cocoon/provider"
)

// CollectFunc returns the live VM and node stats for a single scrape.
type CollectFunc func() ([]provider.VMStats, provider.NodeStats)

// VMCollector is a prometheus.Collector that reads live VM and node stats from a provider callback on each scrape.
type VMCollector struct {
	collectFn CollectFunc

	vmCPUDesc          *prometheus.Desc
	vmThrottledDesc    *prometheus.Desc
	vmThrottledPerDesc *prometheus.Desc
	vmMemDesc          *prometheus.Desc
	vmDiskDesc         *prometheus.Desc
	vmNetRxDesc        *prometheus.Desc
	vmNetTxDesc        *prometheus.Desc
	nodeCPUDesc        *prometheus.Desc
	nodeMemDesc        *prometheus.Desc
	nodeStorAvail      *prometheus.Desc
	nodeStorTotal      *prometheus.Desc
}

// NewVMCollector creates a collector. collectFn is called on every scrape.
func NewVMCollector(collectFn CollectFunc) *VMCollector {
	labels := []string{"vm", "pod", "namespace", "backend"}
	name := func(n string) string { return prometheus.BuildFQName(metricNamespace, metricSubsystem, n) }
	return &VMCollector{
		collectFn:          collectFn,
		vmCPUDesc:          prometheus.NewDesc(name("vm_cpu_seconds_total"), "Cumulative CPU time consumed by the VM in seconds.", labels, nil),
		vmThrottledDesc:    prometheus.NewDesc(name("vm_cpu_throttled_seconds_total"), "Total time the VM cgroup spent throttled by its CPU quota in seconds.", labels, nil),
		vmThrottledPerDesc: prometheus.NewDesc(name("vm_cpu_throttled_periods_total"), "Number of enforcement periods the VM cgroup was throttled in.", labels, nil),
		vmMemDesc:          prometheus.NewDesc(name("vm_memory_rss_bytes"), "Resident set size of the VM hypervisor process in bytes.", labels, nil),
		vmDiskDesc:         prometheus.NewDesc(name("vm_disk_cow_bytes"), "Actual size of the VM COW overlay in bytes.", labels, nil),
		vmNetRxDesc:        prometheus.NewDesc(name("vm_network_rx_bytes_total"), "Total bytes received by the VM TAP device.", labels, nil),
		vmNetTxDesc:        prometheus.NewDesc(name("vm_network_tx_bytes_total"), "Total bytes transmitted by the VM TAP device.", labels, nil),
		nodeCPUDesc:        prometheus.NewDesc(name("node_cpu_seconds_total"), "Cumulative CPU time consumed by the node in seconds.", nil, nil),
		nodeMemDesc:        prometheus.NewDesc(name("node_memory_used_bytes"), "Memory used by the node (MemTotal - MemAvailable) in bytes.", nil, nil),
		nodeStorAvail:      prometheus.NewDesc(name("node_storage_available_bytes"), "Available storage on the cocoon root filesystem in bytes.", nil, nil),
		nodeStorTotal:      prometheus.NewDesc(name("node_storage_total_bytes"), "Total storage on the cocoon root filesystem in bytes.", nil, nil),
	}
}

func (c *VMCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.vmCPUDesc
	ch <- c.vmThrottledDesc
	ch <- c.vmThrottledPerDesc
	ch <- c.vmMemDesc
	ch <- c.vmDiskDesc
	ch <- c.vmNetRxDesc
	ch <- c.vmNetTxDesc
	ch <- c.nodeCPUDesc
	ch <- c.nodeMemDesc
	ch <- c.nodeStorAvail
	ch <- c.nodeStorTotal
}

func (c *VMCollector) Collect(ch chan<- prometheus.Metric) {
	vms, node := c.collectFn()

	for _, v := range vms {
		labels := []string{v.VMName, v.PodName, v.Namespace, v.Backend}
		ch <- prometheus.MustNewConstMetric(c.vmCPUDesc, prometheus.CounterValue, v.CPUSeconds, labels...)
		ch <- prometheus.MustNewConstMetric(c.vmThrottledDesc, prometheus.CounterValue, v.CPUThrottledSeconds, labels...)
		ch <- prometheus.MustNewConstMetric(c.vmThrottledPerDesc, prometheus.CounterValue, float64(v.CPUThrottledPeriods), labels...)
		ch <- prometheus.MustNewConstMetric(c.vmMemDesc, prometheus.GaugeValue, float64(v.MemoryRSS), labels...)
		ch <- prometheus.MustNewConstMetric(c.vmDiskDesc, prometheus.GaugeValue, float64(v.DiskCOW), labels...)
		ch <- prometheus.MustNewConstMetric(c.vmNetRxDesc, prometheus.CounterValue, float64(v.NetRxBytes), labels...)
		ch <- prometheus.MustNewConstMetric(c.vmNetTxDesc, prometheus.CounterValue, float64(v.NetTxBytes), labels...)
	}

	ch <- prometheus.MustNewConstMetric(c.nodeCPUDesc, prometheus.CounterValue, node.CPUSeconds)
	ch <- prometheus.MustNewConstMetric(c.nodeMemDesc, prometheus.GaugeValue, float64(node.MemoryUsedBytes))
	ch <- prometheus.MustNewConstMetric(c.nodeStorAvail, prometheus.GaugeValue, float64(node.StorageAvailable))
	ch <- prometheus.MustNewConstMetric(c.nodeStorTotal, prometheus.GaugeValue, float64(node.StorageTotal))
}
