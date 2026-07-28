package cocoon

import (
	"testing"
	"time"

	"github.com/cocoonstack/vk-cocoon/provider"
)

func TestParseProcStat(t *testing.T) {
	// comm carries spaces and parens; fields after it: state=R, then 20+ numeric.
	line := "1234 (cloud (hv) proc) R 1 1 1 0 -1 4194560 100 0 0 0 250 150 0 0 20 0 4 0 12345 999424 512 18446744073709551615"
	cpu, rss := parseProcStat(line, 4096)
	if cpu != 4.0 {
		t.Errorf("cpu = %v, want 4.0 ((250+150)/100)", cpu)
	}
	if rss != 512*4096 {
		t.Errorf("rss = %d, want %d", rss, 512*4096)
	}
}

func TestParseProcStatMalformed(t *testing.T) {
	for _, s := range []string{"", "no comm here", "1 (x) R 1 2 3"} {
		if cpu, rss := parseProcStat(s, 4096); cpu != 0 || rss != 0 {
			t.Errorf("parseProcStat(%q) = %v,%d, want zeros", s, cpu, rss)
		}
	}
}

func TestSampleStatsServesCachedWithinTTL(t *testing.T) {
	p := newTestProvider(t)
	seeded := []vmSample{{vmSnapshot: vmSnapshot{VMName: "vk-ns-demo-0"}, cpuSeconds: 7}}
	p.statsVMs, p.statsNode, p.statsAt = seeded, provider.NodeStats{CPUSeconds: 42}, time.Now()

	vms, node := p.sampleStats()
	if len(vms) != 1 || vms[0].cpuSeconds != 7 || node.CPUSeconds != 42 {
		t.Fatalf("within TTL must serve the cached sample, got %+v node %+v", vms, node)
	}

	p.statsAt = time.Now().Add(-2 * statsSampleTTL)
	vms, _ = p.sampleStats()
	if len(vms) != 0 {
		t.Fatalf("expired TTL must resample (no tracked VMs), got %+v", vms)
	}
}
