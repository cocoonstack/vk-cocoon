package provider

import (
	"os"
	"runtime"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
)

func TestReserveQuantity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		q    resource.Quantity
		pct  int
		want int64
	}{
		{name: "20% of 100", q: resource.MustParse("100"), pct: 20, want: 80},
		{name: "0% keeps full", q: resource.MustParse("32"), pct: 0, want: 32},
		{name: "100% gives zero", q: resource.MustParse("8Gi"), pct: 100, want: 0},
		{name: "20% of 128Gi", q: resource.MustParse("128Gi"), pct: 20, want: 128 * 1024 * 1024 * 1024 * 80 / 100},
		{name: "20% of 32 CPU", q: resource.MustParse("32"), pct: 20, want: 25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reserveQuantity(tc.q, tc.pct)
			if got.Value() != tc.want {
				t.Errorf("reserveQuantity(%v, %d) = %d, want %d", tc.q, tc.pct, got.Value(), tc.want)
			}
		})
	}
}

func TestNodeResourcesDefaults(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires /proc")
	}
	rootDir := commonk8s.EnvOrDefault("COCOON_ROOT_DIR", "/var/lib/cocoon")
	if _, err := os.Stat(rootDir); err != nil {
		t.Skipf("requires %s for statfs", rootDir)
	}
	cap, alloc, err := NodeResources()
	if err != nil {
		t.Fatalf("NodeResources: %v", err)
	}

	cpu := cap.Cpu()
	if cpu.IsZero() {
		t.Errorf("capacity CPU is zero")
	}
	mem := cap.Memory()
	if mem.IsZero() {
		t.Errorf("capacity Memory is zero")
	}

	allocCPU := alloc.Cpu()
	if allocCPU.Cmp(*cpu) >= 0 {
		t.Errorf("allocatable CPU %v should be less than capacity %v", allocCPU, cpu)
	}
	allocMem := alloc.Memory()
	if allocMem.Cmp(*mem) >= 0 {
		t.Errorf("allocatable Memory %v should be less than capacity %v", allocMem, mem)
	}
}
