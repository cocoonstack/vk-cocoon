package provider

import (
	"context"
	"maps"
	"strings"

	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

func (p *CocoonProvider) GetStatsSummary(ctx context.Context) (*statsv1alpha1.Summary, error) {
	now := metav1.Now()
	pods := make([]statsv1alpha1.PodStats, 0)

	p.mu.RLock()
	vmsCopy := make(map[string]*CocoonVM, len(p.vms))
	maps.Copy(vmsCopy, p.vms)
	p.mu.RUnlock()

	for key, vm := range vmsCopy {
		ns, name, ok := strings.Cut(key, "/")
		if !ok || vm.vmID == "" {
			continue
		}

		podStat := statsv1alpha1.PodStats{
			PodRef:    statsv1alpha1.PodReference{Name: name, Namespace: ns},
			StartTime: metav1.NewTime(vm.createdAt),
		}

		if !strings.HasPrefix(vm.vmID, "static-") { //nolint:nestif // metrics collection from multiple APIs
			if ping, err := chGetPing(ctx, vm.vmID); err == nil && ping.PID > 0 {
				if uNs, sNs, err := readProcCPUUsage(ping.PID); err == nil {
					totalNs := uNs + sNs
					podStat.CPU = &statsv1alpha1.CPUStats{
						Time:                 now,
						UsageCoreNanoSeconds: &totalNs,
					}
				}
				if rss, err := readProcMemoryRSS(ping.PID); err == nil {
					podStat.Memory = &statsv1alpha1.MemoryStats{
						Time:            now,
						WorkingSetBytes: &rss,
					}
				}
			}
			if counters, err := chGetCounters(ctx, vm.vmID); err == nil {
				for devName, stats := range counters {
					if strings.Contains(devName, "net") {
						rx := stats["rx_bytes"]
						tx := stats["tx_bytes"]
						podStat.Network = &statsv1alpha1.NetworkStats{
							Time: now,
							Interfaces: []statsv1alpha1.InterfaceStats{{
								Name:    devName,
								RxBytes: &rx,
								TxBytes: &tx,
							}},
						}
					}
				}
			}
		} else {
			cpuNano := uint64(vm.cpu) * 1e9               //nolint:gosec // CPU count is always small positive
			memBytes := uint64(vm.memoryMB) * 1024 * 1024 //nolint:gosec // MemoryMB is always positive
			podStat.CPU = &statsv1alpha1.CPUStats{Time: now, UsageCoreNanoSeconds: &cpuNano}
			podStat.Memory = &statsv1alpha1.MemoryStats{Time: now, WorkingSetBytes: &memBytes}
		}
		pods = append(pods, podStat)
	}

	nodeCPU := uint64(0)
	nodeMem := readHostMemoryBytes()
	return &statsv1alpha1.Summary{
		Node: statsv1alpha1.NodeStats{
			NodeName:  "cocoon-pool",
			StartTime: now,
			CPU:       &statsv1alpha1.CPUStats{Time: now, UsageCoreNanoSeconds: &nodeCPU},
			Memory:    &statsv1alpha1.MemoryStats{Time: now, WorkingSetBytes: &nodeMem},
		},
		Pods: pods,
	}, nil
}

func (p *CocoonProvider) GetMetricsResource(ctx context.Context) ([]*dto.MetricFamily, error) {
	p.mu.RLock()
	vmsCopy := make(map[string]*CocoonVM, len(p.vms))
	maps.Copy(vmsCopy, p.vms)
	p.mu.RUnlock()

	gauge := dto.MetricType_GAUGE
	families := make([]*dto.MetricFamily, 0, 4)

	cpuName := "pod_cpu_usage_seconds_total"
	memName := "pod_memory_working_set_bytes"
	netRxName := "pod_network_receive_bytes_total"
	netTxName := "pod_network_transmit_bytes_total"

	cpuMetrics := make([]*dto.Metric, 0)
	memMetrics := make([]*dto.Metric, 0)
	netRxMetrics := make([]*dto.Metric, 0)
	netTxMetrics := make([]*dto.Metric, 0)

	for key, vm := range vmsCopy {
		ns, name, ok := strings.Cut(key, "/")
		if !ok || vm.vmID == "" {
			continue
		}
		nsLabel := "namespace"
		podLabel := "pod"
		labels := []*dto.LabelPair{
			{Name: &nsLabel, Value: &ns},
			{Name: &podLabel, Value: &name},
		}

		if !strings.HasPrefix(vm.vmID, "static-") { //nolint:nestif // per-device metrics from CH API
			if ping, err := chGetPing(ctx, vm.vmID); err == nil && ping.PID > 0 {
				if uNs, sNs, err := readProcCPUUsage(ping.PID); err == nil {
					cpuSec := float64(uNs+sNs) / 1e9
					cpuMetrics = append(cpuMetrics, &dto.Metric{Label: labels, Gauge: &dto.Gauge{Value: &cpuSec}})
				}
				if rss, err := readProcMemoryRSS(ping.PID); err == nil {
					memF := float64(rss)
					memMetrics = append(memMetrics, &dto.Metric{Label: labels, Gauge: &dto.Gauge{Value: &memF}})
				}
			}
			if counters, err := chGetCounters(ctx, vm.vmID); err == nil {
				for devName, stats := range counters {
					if strings.Contains(devName, "net") {
						rx := float64(stats["rx_bytes"])
						tx := float64(stats["tx_bytes"])
						if rx < 1e18 {
							netRxMetrics = append(netRxMetrics, &dto.Metric{Label: labels, Gauge: &dto.Gauge{Value: &rx}})
						}
						if tx < 1e18 {
							netTxMetrics = append(netTxMetrics, &dto.Metric{Label: labels, Gauge: &dto.Gauge{Value: &tx}})
						}
					}
				}
			}
		}
	}

	if len(cpuMetrics) > 0 {
		families = append(families, &dto.MetricFamily{Name: &cpuName, Type: &gauge, Metric: cpuMetrics})
	}
	if len(memMetrics) > 0 {
		families = append(families, &dto.MetricFamily{Name: &memName, Type: &gauge, Metric: memMetrics})
	}
	if len(netRxMetrics) > 0 {
		families = append(families, &dto.MetricFamily{Name: &netRxName, Type: &gauge, Metric: netRxMetrics})
	}
	if len(netTxMetrics) > 0 {
		families = append(families, &dto.MetricFamily{Name: &netTxName, Type: &gauge, Metric: netTxMetrics})
	}
	return families, nil
}
