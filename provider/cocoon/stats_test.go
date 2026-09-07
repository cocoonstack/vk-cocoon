package cocoon

import (
	"slices"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/cocoonstack/vk-cocoon/provider"
)

func TestParseProcStatRSS(t *testing.T) {
	line := "1234 (cloud (hv) proc) R 1 1 1 0 -1 4194560 100 0 0 0 250 150 0 0 20 0 4 0 12345 999424 512 18446744073709551615"
	if rss := parseProcStatRSS(line, 4096); rss != 512*4096 {
		t.Errorf("rss = %d, want %d", rss, 512*4096)
	}
}

func TestParseProcStatRSSMalformed(t *testing.T) {
	for _, s := range []string{"", "no comm here", "1 (x) R 1 2 3"} {
		if rss := parseProcStatRSS(s, 4096); rss != 0 {
			t.Errorf("parseProcStatRSS(%q) = %d, want 0", s, rss)
		}
	}
}

func TestParseProcStatCPUSeconds(t *testing.T) {
	line := "1234 (qemu (vm) proc) R 1 1 1 0 -1 4194560 100 0 0 0 250 150 0 0 20 0 4 0 12345 999424 512 18446744073709551615"
	if got := parseProcStatCPUSeconds(line); got != 4 {
		t.Errorf("cpu seconds = %v, want 4 (utime 250 + stime 150 over USER_HZ)", got)
	}
	for _, s := range []string{"", "no comm here", "1 (x) R 1 2 3"} {
		if got := parseProcStatCPUSeconds(s); got != 0 {
			t.Errorf("parseProcStatCPUSeconds(%q) = %v, want 0", s, got)
		}
	}
}

func TestStatsReportThePodStartTime(t *testing.T) {
	p := newTestProvider(t)
	started := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	p.statsVMs = []provider.VMStats{{VMName: "vk-ns-demo-0", PodName: "demo-0", Namespace: "ns", StartedAt: started}}
	p.statsAt = time.Now()

	summary, err := p.GetStatsSummary(t.Context())
	if err != nil {
		t.Fatalf("GetStatsSummary: %v", err)
	}
	pod := summary.Pods[0]
	if !pod.StartTime.Time.Equal(started) || !pod.Containers[0].StartTime.Time.Equal(started) {
		t.Fatalf("pod start %v container start %v, want %v", pod.StartTime.Time, pod.Containers[0].StartTime.Time, started)
	}

	families, err := p.GetMetricsResource(t.Context())
	if err != nil {
		t.Fatalf("GetMetricsResource: %v", err)
	}
	i := slices.IndexFunc(families, func(f *dto.MetricFamily) bool { return f.GetName() == "container_start_time_seconds" })
	if i < 0 {
		t.Fatal("container_start_time_seconds family missing")
	}
	if got := families[i].Metric[0].GetGauge().GetValue(); got != float64(started.Unix()) {
		t.Fatalf("container_start_time_seconds = %v, want %v", got, started.Unix())
	}
}

func TestSampleStatsServesCachedWithinTTL(t *testing.T) {
	p := newTestProvider(t)
	seeded := []provider.VMStats{{VMName: "vk-ns-demo-0", CPUSeconds: 7}}
	p.statsVMs, p.statsNode, p.statsAt = seeded, provider.NodeStats{CPUSeconds: 42}, time.Now()

	vms, node := p.CollectVMStats()
	if len(vms) != 1 || vms[0].CPUSeconds != 7 || node.CPUSeconds != 42 {
		t.Fatalf("within TTL must serve the cached sample, got %+v node %+v", vms, node)
	}

	p.statsAt = time.Now().Add(-2 * statsSampleTTL)
	vms, _ = p.CollectVMStats()
	if len(vms) != 0 {
		t.Fatalf("expired TTL must resample (no tracked VMs), got %+v", vms)
	}
}
