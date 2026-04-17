package cocoon

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	"github.com/cocoonstack/cocoon-common/meta"
)

// vmSnapshot is a minimal copy of VM state taken under lock so /proc
// reads happen outside the critical section.
type vmSnapshot struct {
	ID         string
	PID        int
	Tap        string
	Hypervisor string
	Backend    string
	Namespace  string
	PodName    string
	VMName     string
}

// snapshotTrackedVMs copies the minimal VM data needed for stats under
// RLock, then releases it so /proc reads don't block CreatePod/DeletePod.
func (p *Provider) snapshotTrackedVMs() []vmSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]vmSnapshot, 0, len(p.pods))
	for key, pod := range p.pods {
		spec := meta.ParseVMSpec(pod)
		v := p.vmsByName[spec.VMName]
		if v == nil || v.PID == 0 {
			continue
		}
		ns, name := splitPodKey(key)
		snap := vmSnapshot{
			ID: v.ID, PID: v.PID,
			Hypervisor: v.Hypervisor, Backend: spec.Backend,
			VMName: spec.VMName, Namespace: ns, PodName: name,
		}
		if len(v.NetworkConfigs) > 0 {
			snap.Tap = v.NetworkConfigs[0].Tap
		}
		out = append(out, snap)
	}
	return out
}

// GetStatsSummary returns a kubelet-compatible stats summary with real
// resource usage. metrics-server and kubectl top consume this endpoint.
func (p *Provider) GetStatsSummary(_ context.Context) (*statsv1alpha1.Summary, error) {
	now := metav1.NewTime(time.Now())
	nodeCPU, nodeMemory := readNodeUsage()

	snapshots := p.snapshotTrackedVMs()
	podStats := make([]statsv1alpha1.PodStats, 0, len(snapshots))
	for _, s := range snapshots {
		cpu, mem := readProcessUsage(s.PID)
		ps := statsv1alpha1.PodStats{
			PodRef:    statsv1alpha1.PodReference{Name: s.PodName, Namespace: s.Namespace},
			StartTime: now,
			Containers: []statsv1alpha1.ContainerStats{{
				Name: "agent", StartTime: now, CPU: cpu, Memory: mem,
			}},
		}
		if net := buildNetworkStats(s.PID, s.Tap); net != nil {
			ps.Network = net
		}
		podStats = append(podStats, ps)
	}

	return &statsv1alpha1.Summary{
		Node: statsv1alpha1.NodeStats{
			NodeName:  p.NodeName,
			StartTime: metav1.NewTime(p.startTime),
			CPU:       nodeCPU,
			Memory:    nodeMemory,
		},
		Pods: podStats,
	}, nil
}

// GetMetricsResource returns kubelet /metrics/resource prometheus metrics.
func (p *Provider) GetMetricsResource(_ context.Context) ([]*dto.MetricFamily, error) {
	nowMs := time.Now().UnixMilli()

	families := []*dto.MetricFamily{
		newCounterFamily("node_cpu_usage_seconds_total",
			"Cumulative cpu time consumed by the node in core-seconds",
			newCounter(readNodeCPUSeconds(), nowMs, nil)),
		newGaugeFamily("node_memory_working_set_bytes",
			"Current working set of the node in bytes",
			newGauge(float64(readNodeMemoryWorkingSet()), nowMs, nil)),
	}

	snapshots := p.snapshotTrackedVMs()
	var cpuMetrics, memMetrics []*dto.Metric
	for _, s := range snapshots {
		labels := []*dto.LabelPair{
			{Name: proto.String("namespace"), Value: proto.String(s.Namespace)},
			{Name: proto.String("pod"), Value: proto.String(s.PodName)},
			{Name: proto.String("container"), Value: proto.String("agent")},
		}
		cpuMetrics = append(cpuMetrics, newCounter(readProcessCPUSeconds(s.PID), nowMs, labels))
		memMetrics = append(memMetrics, newGauge(float64(readProcessMemoryWorkingSet(s.PID)), nowMs, labels))
	}

	if len(cpuMetrics) > 0 {
		families = append(families,
			newCounterFamily("container_cpu_usage_seconds_total",
				"Cumulative cpu time consumed by the container in core-seconds", cpuMetrics...),
			newGaugeFamily("container_memory_working_set_bytes",
				"Current working set of the container in bytes", memMetrics...),
		)
	}
	return families, nil
}

func buildNetworkStats(pid int, tap string) *statsv1alpha1.NetworkStats {
	if tap == "" || pid == 0 {
		return nil
	}
	rx, tx := readProcNetDev(pid, tap)
	if rx == 0 && tx == 0 {
		return nil
	}
	return &statsv1alpha1.NetworkStats{
		InterfaceStats: statsv1alpha1.InterfaceStats{
			Name: tap, RxBytes: &rx, TxBytes: &tx,
		},
	}
}

func splitPodKey(key string) (string, string) {
	ns, name, _ := strings.Cut(key, "/")
	return ns, name
}

func readNodeUsage() (*statsv1alpha1.CPUStats, *statsv1alpha1.MemoryStats) {
	cpuNano := uint64(readNodeCPUSeconds() * 1e9)          //nolint:gosec
	memBytes := uint64(max(readNodeMemoryWorkingSet(), 0)) //nolint:gosec
	return &statsv1alpha1.CPUStats{UsageCoreNanoSeconds: &cpuNano},
		&statsv1alpha1.MemoryStats{WorkingSetBytes: &memBytes}
}

func readNodeCPUSeconds() float64 {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer f.Close() //nolint:errcheck
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		var total int64
		for _, s := range strings.Fields(line)[1:] {
			v, _ := strconv.ParseInt(s, 10, 64)
			total += v
		}
		return float64(total) / 100 // USER_HZ
	}
	return 0
}

func readNodeMemoryWorkingSet() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close() //nolint:errcheck
	var total, avail int64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			total = parseMemInfoValue(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			avail = parseMemInfoValue(line)
		}
	}
	return (total - avail) * 1024
}

func readProcessUsage(pid int) (*statsv1alpha1.CPUStats, *statsv1alpha1.MemoryStats) {
	cpuNano := uint64(readProcessCPUSeconds(pid) * 1e9)          //nolint:gosec
	memBytes := uint64(max(readProcessMemoryWorkingSet(pid), 0)) //nolint:gosec
	return &statsv1alpha1.CPUStats{UsageCoreNanoSeconds: &cpuNano},
		&statsv1alpha1.MemoryStats{WorkingSetBytes: &memBytes}
}

func readProcessCPUSeconds(pid int) float64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	idx := strings.LastIndex(string(data), ")")
	if idx < 0 || idx+2 >= len(data) {
		return 0
	}
	fields := strings.Fields(string(data)[idx+2:])
	if len(fields) < 13 {
		return 0
	}
	utime, _ := strconv.ParseInt(fields[11], 10, 64)
	stime, _ := strconv.ParseInt(fields[12], 10, 64)
	return float64(utime+stime) / 100 // CLK_TCK
}

func readProcessMemoryWorkingSet(pid int) int64 {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	defer f.Close() //nolint:errcheck
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "VmRSS:") {
			return parseMemInfoValue(scanner.Text()) * 1024
		}
	}
	return 0
}

func parseMemInfoValue(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseInt(fields[1], 10, 64)
	return v
}

func newCounterFamily(name, help string, metrics ...*dto.Metric) *dto.MetricFamily {
	t := dto.MetricType_COUNTER
	return &dto.MetricFamily{Name: &name, Help: &help, Type: &t, Metric: metrics}
}

func newGaugeFamily(name, help string, metrics ...*dto.Metric) *dto.MetricFamily {
	t := dto.MetricType_GAUGE
	return &dto.MetricFamily{Name: &name, Help: &help, Type: &t, Metric: metrics}
}

func newCounter(value float64, timestampMs int64, labels []*dto.LabelPair) *dto.Metric {
	return &dto.Metric{Label: labels, Counter: &dto.Counter{Value: &value}, TimestampMs: &timestampMs}
}

func newGauge(value float64, timestampMs int64, labels []*dto.LabelPair) *dto.Metric {
	return &dto.Metric{Label: labels, Gauge: &dto.Gauge{Value: &value}, TimestampMs: &timestampMs}
}
