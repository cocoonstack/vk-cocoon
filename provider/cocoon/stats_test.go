package cocoon

import (
	"testing"
	"time"

	"github.com/cocoonstack/vk-cocoon/provider"
)

func TestParseProcStatRSS(t *testing.T) {
	// comm carries spaces and parens; fields after it: state=R, then 20+ numeric.
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

func TestParseCPUStat(t *testing.T) {
	data := "usage_usec 2500000\nuser_usec 2000000\nsystem_usec 500000\nnr_periods 100\nnr_throttled 7\nthrottled_usec 1500000\n"
	usageUs, throttledUs, nrThrottled := parseCPUStat(data)
	if usageUs != 2500000 || throttledUs != 1500000 || nrThrottled != 7 {
		t.Errorf("parseCPUStat = %d,%d,%d, want 2500000,1500000,7", usageUs, throttledUs, nrThrottled)
	}
}

func TestParseCPUStatMalformed(t *testing.T) {
	for _, s := range []string{"", "usage_usec", "usage_usec abc\nnr_throttled -"} {
		if usageUs, throttledUs, nrThrottled := parseCPUStat(s); usageUs != 0 || throttledUs != 0 || nrThrottled != 0 {
			t.Errorf("parseCPUStat(%q) = %d,%d,%d, want zeros", s, usageUs, throttledUs, nrThrottled)
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
