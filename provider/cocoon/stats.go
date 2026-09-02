package cocoon

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/provider"
	"github.com/cocoonstack/vk-cocoon/vm"
)

const statsSampleTTL = 2 * time.Second

// cgroupParent must match cocoon's cgroup_parent config; override via COCOON_CGROUP_PARENT when cocoon's does.
var cgroupParent = sync.OnceValue(func() string {
	return commonk8s.EnvOrDefault("COCOON_CGROUP_PARENT", vm.DefaultCgroupParent)
})

// vmSnapshot is a minimal copy of VM state taken under lock so /proc reads happen outside the critical section.
type vmSnapshot struct {
	ID         string
	PID        int
	Tap        string
	DiskPath   string
	Hypervisor string
	Backend    string
	Namespace  string
	PodName    string
	VMName     string
}

// vmSample is one cached stats reading for a tracked VM.
type vmSample struct {
	vmSnapshot
	cpuSeconds       float64
	throttledSeconds float64
	nrThrottled      int64
	memBytes         int64
	diskCOW          int64
	rxBytes          uint64
	txBytes          uint64
}

// metrics-server and kubectl top consume this endpoint.
func (p *Provider) GetStatsSummary(_ context.Context) (*statsv1alpha1.Summary, error) {
	now := metav1.Now()
	samples, node := p.sampleStats()
	nodeCPU, nodeMemory := cpuMemStats(node.CPUSeconds, node.MemoryUsedBytes)

	podStats := make([]statsv1alpha1.PodStats, 0, len(samples))
	for _, s := range samples {
		cpu, mem := cpuMemStats(s.cpuSeconds, s.memBytes)
		ps := statsv1alpha1.PodStats{
			PodRef:    statsv1alpha1.PodReference{Name: s.PodName, Namespace: s.Namespace},
			StartTime: now,
			Containers: []statsv1alpha1.ContainerStats{{
				Name: containerName, StartTime: now, CPU: cpu, Memory: mem,
			}},
		}
		if net := buildNetworkStats(s); net != nil {
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

func (p *Provider) GetMetricsResource(_ context.Context) ([]*dto.MetricFamily, error) {
	nowMs := time.Now().UnixMilli()
	samples, node := p.sampleStats()

	families := []*dto.MetricFamily{
		newCounterFamily("node_cpu_usage_seconds_total",
			"Cumulative cpu time consumed by the node in core-seconds",
			newCounter(node.CPUSeconds, nowMs, nil)),
		newGaugeFamily("node_memory_working_set_bytes",
			"Current working set of the node in bytes",
			newGauge(float64(node.MemoryUsedBytes), nowMs, nil)),
	}

	var containerCPU, containerMem, podCPU, podMem, throttledSec, throttledPeriods []*dto.Metric
	for _, s := range samples {
		cpuSec := s.cpuSeconds
		memBytes := float64(s.memBytes)

		containerLabels := []*dto.LabelPair{
			{Name: new("namespace"), Value: new(s.Namespace)},
			{Name: new("pod"), Value: new(s.PodName)},
			{Name: new("container"), Value: new(containerName)},
		}
		containerCPU = append(containerCPU, newCounter(cpuSec, nowMs, containerLabels))
		containerMem = append(containerMem, newGauge(memBytes, nowMs, containerLabels))
		throttledSec = append(throttledSec, newCounter(s.throttledSeconds, nowMs, containerLabels))
		throttledPeriods = append(throttledPeriods, newCounter(float64(s.nrThrottled), nowMs, containerLabels))

		podLabels := []*dto.LabelPair{
			{Name: new("namespace"), Value: new(s.Namespace)},
			{Name: new("pod"), Value: new(s.PodName)},
		}
		podCPU = append(podCPU, newCounter(cpuSec, nowMs, podLabels))
		podMem = append(podMem, newGauge(memBytes, nowMs, podLabels))
	}

	if len(containerCPU) > 0 {
		families = append(
			families,
			newCounterFamily("container_cpu_usage_seconds_total",
				"Cumulative cpu time consumed by the container in core-seconds", containerCPU...),
			newGaugeFamily("container_memory_working_set_bytes",
				"Current working set of the container in bytes", containerMem...),
			newCounterFamily("container_cpu_cfs_throttled_seconds_total",
				"Total time the container's VM cgroup spent throttled in seconds", throttledSec...),
			newCounterFamily("container_cpu_cfs_throttled_periods_total",
				"Number of throttled enforcement periods of the container's VM cgroup", throttledPeriods...),
			newCounterFamily("pod_cpu_usage_seconds_total",
				"Cumulative cpu time consumed by the pod in core-seconds", podCPU...),
			newGaugeFamily("pod_memory_working_set_bytes",
				"Current working set of the pod in bytes", podMem...),
		)
	}

	return families, nil
}

// sampleStats returns the shared short-TTL scrape sample: three consumers (stats summary, metrics, Prometheus) land independently on it.
func (p *Provider) sampleStats() ([]vmSample, provider.NodeStats) {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	if time.Since(p.statsAt) < statsSampleTTL {
		return p.statsVMs, p.statsNode
	}
	snaps := p.snapshotTrackedVMs()
	vms := make([]vmSample, 0, len(snaps))
	for _, s := range snaps {
		sample := vmSample{vmSnapshot: s}
		if s.Hypervisor == macosHypervisor {
			// qemu runs outside cocoon's cgroup slice and run-dir.
			stat := readProcStat(s.PID)
			sample.cpuSeconds = parseProcStatCPUSeconds(stat)
			sample.memBytes = parseProcStatRSS(stat, os.Getpagesize())
			sample.diskCOW = fileSize(s.DiskPath)
		} else {
			sample.memBytes = readProcRSS(s.PID)
			// CPU from the cgroup scope, not /proc: the VMM's utime/stime never sees the virtio and io_uring kernel workers.
			sample.cpuSeconds, sample.throttledSeconds, sample.nrThrottled = vm.ScopeCPUStat(cgroupParent(), s.ID)
			sample.diskCOW = vm.COWSize(provider.CocoonRootDir(), s.Hypervisor, s.ID)
		}
		if s.Tap != "" {
			sample.rxBytes, sample.txBytes = readProcNetDev(s.PID, s.Tap)
		}
		vms = append(vms, sample)
	}
	node := provider.NodeStats{
		CPUSeconds:      readNodeCPUSeconds(),
		MemoryUsedBytes: readNodeMemoryWorkingSet(),
	}
	node.StorageTotal, node.StorageAvailable = provider.StorageBytes()
	p.statsVMs, p.statsNode, p.statsAt = vms, node, time.Now()
	return vms, node
}

// snapshotTrackedVMs copies the minimal VM data under RLock, then releases it so /proc reads don't block CreatePod/DeletePod.
func (p *Provider) snapshotTrackedVMs() []vmSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]vmSnapshot, 0, len(p.pods))
	for key, pod := range p.pods {
		spec := meta.ParseVMSpec(pod)
		v := p.vmsByPod[key]
		if v == nil || v.PID == 0 {
			continue
		}
		snap := vmSnapshot{
			ID: v.ID, PID: v.PID, DiskPath: v.DiskPath,
			Hypervisor: v.Hypervisor, Backend: spec.Backend,
			VMName: spec.VMName, Namespace: pod.Namespace, PodName: pod.Name,
		}
		if len(v.NetworkConfigs) > 0 {
			snap.Tap = v.NetworkConfigs[0].Tap
		}
		out = append(out, snap)
	}
	return out
}

func buildNetworkStats(s vmSample) *statsv1alpha1.NetworkStats {
	if s.rxBytes == 0 && s.txBytes == 0 {
		return nil
	}
	rx, tx := s.rxBytes, s.txBytes
	return &statsv1alpha1.NetworkStats{
		Name: s.Tap, RxBytes: &rx, TxBytes: &tx,
	}
}

func readNodeCPUSeconds() float64 {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer f.Close() //nolint:errcheck // read-only file handle, close error is informational
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
	fields, err := provider.ReadKeyedProcFile("/proc/meminfo", "MemTotal", "MemAvailable")
	if err != nil {
		return 0
	}
	return (fields["MemTotal"] - fields["MemAvailable"]) * 1024
}

// cpuMemStats packs raw CPU seconds and working-set bytes into the kubelet stats types, clamping memory to non-negative.
func cpuMemStats(cpuSeconds float64, memBytes int64) (*statsv1alpha1.CPUStats, *statsv1alpha1.MemoryStats) {
	cpuNano := uint64(cpuSeconds * 1e9) //nolint:gosec // cpu seconds read from /proc are always non-negative
	mem := uint64(max(memBytes, 0))     //nolint:gosec // clamped to non-negative via max
	return &statsv1alpha1.CPUStats{UsageCoreNanoSeconds: &cpuNano},
		&statsv1alpha1.MemoryStats{WorkingSetBytes: &mem}
}

func readProcRSS(pid int) int64 {
	return parseProcStatRSS(readProcStat(pid), os.Getpagesize())
}

func readProcStat(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	return string(data)
}

// procStatFields splits a /proc/<pid>/stat line after the parenthesized comm; proc(5) field N lands at index N-3.
func procStatFields(s string) []string {
	idx := strings.LastIndex(s, ")")
	if idx < 0 || idx+2 >= len(s) {
		return nil
	}
	return strings.Fields(s[idx+2:])
}

func parseProcStatCPUSeconds(s string) float64 {
	fields := procStatFields(s)
	if len(fields) < 13 {
		return 0
	}
	utime, _ := strconv.ParseFloat(fields[11], 64)
	stime, _ := strconv.ParseFloat(fields[12], 64)
	return (utime + stime) / 100 // USER_HZ
}

func fileSize(path string) int64 {
	if path == "" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func parseProcStatRSS(s string, pageSize int) int64 {
	fields := procStatFields(s)
	if len(fields) < 22 {
		return 0
	}
	rssPages, _ := strconv.ParseInt(fields[21], 10, 64)
	return rssPages * int64(pageSize)
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
