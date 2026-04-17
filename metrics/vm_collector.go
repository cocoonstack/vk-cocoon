package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/cocoonstack/vk-cocoon/provider"
)

// VMCollector is a prometheus.Collector that reads live VM and node
// stats on each scrape. The provider supplies a callback that returns
// the current state.
type VMCollector struct {
	collectFn func() ([]provider.VMStats, provider.NodeStats)

	vmCPUDesc     *prometheus.Desc
	vmMemDesc     *prometheus.Desc
	vmDiskDesc    *prometheus.Desc
	vmNetRxDesc   *prometheus.Desc
	vmNetTxDesc   *prometheus.Desc
	nodeCPUDesc   *prometheus.Desc
	nodeMemDesc   *prometheus.Desc
	nodeStorAvail *prometheus.Desc
	nodeStorTotal *prometheus.Desc
}

// NewVMCollector creates a collector. collectFn is called on every scrape.
func NewVMCollector(collectFn func() ([]provider.VMStats, provider.NodeStats)) *VMCollector {
	labels := []string{"vm", "pod", "namespace", "backend"}
	return &VMCollector{
		collectFn:     collectFn,
		vmCPUDesc:     prometheus.NewDesc(metricNamespace+"_vm_cpu_seconds_total", "Cumulative CPU time consumed by the VM in seconds.", labels, nil),
		vmMemDesc:     prometheus.NewDesc(metricNamespace+"_vm_memory_rss_bytes", "Resident set size of the VM hypervisor process in bytes.", labels, nil),
		vmDiskDesc:    prometheus.NewDesc(metricNamespace+"_vm_disk_cow_bytes", "Actual size of the VM COW overlay in bytes.", labels, nil),
		vmNetRxDesc:   prometheus.NewDesc(metricNamespace+"_vm_network_rx_bytes_total", "Total bytes received by the VM TAP device.", labels, nil),
		vmNetTxDesc:   prometheus.NewDesc(metricNamespace+"_vm_network_tx_bytes_total", "Total bytes transmitted by the VM TAP device.", labels, nil),
		nodeCPUDesc:   prometheus.NewDesc(metricNamespace+"_node_cpu_seconds_total", "Cumulative CPU time consumed by the node in seconds.", nil, nil),
		nodeMemDesc:   prometheus.NewDesc(metricNamespace+"_node_memory_used_bytes", "Memory used by the node (MemTotal - MemAvailable) in bytes.", nil, nil),
		nodeStorAvail: prometheus.NewDesc(metricNamespace+"_node_storage_available_bytes", "Available storage on the cocoon root filesystem in bytes.", nil, nil),
		nodeStorTotal: prometheus.NewDesc(metricNamespace+"_node_storage_total_bytes", "Total storage on the cocoon root filesystem in bytes.", nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *VMCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.vmCPUDesc
	ch <- c.vmMemDesc
	ch <- c.vmDiskDesc
	ch <- c.vmNetRxDesc
	ch <- c.vmNetTxDesc
	ch <- c.nodeCPUDesc
	ch <- c.nodeMemDesc
	ch <- c.nodeStorAvail
	ch <- c.nodeStorTotal
}

// Collect implements prometheus.Collector.
func (c *VMCollector) Collect(ch chan<- prometheus.Metric) {
	vms, node := c.collectFn()

	for _, v := range vms {
		labels := []string{v.VMName, v.PodName, v.Namespace, v.Backend}
		ch <- prometheus.MustNewConstMetric(c.vmCPUDesc, prometheus.CounterValue, v.CPUSeconds, labels...)
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
