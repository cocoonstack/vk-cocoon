package vm

import "testing"

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
