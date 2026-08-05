package cocoon

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
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

const (
	statsSampleTTL = 2 * time.Second

	// defaultCgroupParent mirrors cocoon's cgroup_parent default; override
	// via COCOON_CGROUP_PARENT when cocoon's config does.
	defaultCgroupParent = "cocoon.slice"
)

var cgroupParentDir = sync.OnceValue(func() string {
	return filepath.Join("/sys/fs/cgroup", commonk8s.EnvOrDefault("COCOON_CGROUP_PARENT", defaultCgroupParent))
})

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

// sampleStats returns the shared short-TTL scrape sample: three consumers
// (stats summary, metrics resource, Prometheus) land independently on it.
func (p *Provider) sampleStats() ([]vmSample, provider.NodeStats) {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	if time.Since(p.statsAt) < statsSampleTTL {
		return p.statsVMs, p.statsNode
	}
	snaps := p.snapshotTrackedVMs()
	vms := make([]vmSample, 0, len(snaps))
	for _, s := range snaps {
		// CPU from the cgroup scope, not /proc: the VMM's utime/stime never
		// sees the virtio and io_uring kernel workers the scope contains.
		usage, throttledSec, throttled := readScopeCPUStat(s.ID)
		sample := vmSample{
			vmSnapshot: s, cpuSeconds: usage,
			throttledSeconds: throttledSec, nrThrottled: throttled,
			memBytes: readProcRSS(s.PID),
			diskCOW:  vm.COWSize(provider.CocoonRootDir(), s.Hypervisor, s.ID),
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

func buildNetworkStats(s vmSample) *statsv1alpha1.NetworkStats {
	if s.rxBytes == 0 && s.txBytes == 0 {
		return nil
	}
	rx, tx := s.rxBytes, s.txBytes
	return &statsv1alpha1.NetworkStats{
		InterfaceStats: statsv1alpha1.InterfaceStats{
			Name: s.Tap, RxBytes: &rx, TxBytes: &tx,
		},
	}
}

func splitPodKey(key string) (string, string) {
	ns, name, _ := strings.Cut(key, "/")
	return ns, name
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

// cpuMemStats packs raw CPU seconds and working-set bytes into the
// kubelet stats types, clamping memory to non-negative.
func cpuMemStats(cpuSeconds float64, memBytes int64) (*statsv1alpha1.CPUStats, *statsv1alpha1.MemoryStats) {
	cpuNano := uint64(cpuSeconds * 1e9) //nolint:gosec // cpu seconds read from /proc are always non-negative
	mem := uint64(max(memBytes, 0))     //nolint:gosec // clamped to non-negative via max
	return &statsv1alpha1.CPUStats{UsageCoreNanoSeconds: &cpuNano},
		&statsv1alpha1.MemoryStats{WorkingSetBytes: &mem}
}

// readScopeCPUStat reads a VM's cpu.stat from its cocoon cgroup scope;
// zeros when the scope is gone (VM dying) or the parent slice mismatches.
func readScopeCPUStat(vmID string) (usageSeconds, throttledSeconds float64, nrThrottled int64) {
	data, err := os.ReadFile(filepath.Join(cgroupParentDir(), "vm-"+vmID+".scope", "cpu.stat")) //nolint:gosec // path derives from env config + tracked VM ID
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

func readProcRSS(pid int) int64 {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	return parseProcStatRSS(string(data), os.Getpagesize())
}

// parseProcStatRSS extracts RSS from a /proc/<pid>/stat line (field 24;
// split after the parenthesized comm, which may contain spaces).
func parseProcStatRSS(s string, pageSize int) int64 {
	idx := strings.LastIndex(s, ")")
	if idx < 0 || idx+2 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[idx+2:])
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
