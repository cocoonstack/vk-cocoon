package provider

import (
	"os"
	"runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestHugepagesResourceNameFollowsThePageSize(t *testing.T) {
	for pageSizeKB, want := range map[int64]corev1.ResourceName{2048: "hugepages-2Mi", 1048576: "hugepages-1Gi", 0: "hugepages-2Mi"} {
		if got := hugepagesResourceName(pageSizeKB); got != want {
			t.Errorf("hugepagesResourceName(%d) = %q, want %q", pageSizeKB, got, want)
		}
	}
}

func TestDetectHugepagesOverrideFollowsTheHostPageSize(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires /proc/meminfo")
	}
	t.Setenv("VK_NODE_HUGEPAGES", "8Gi")
	fields, err := ReadKeyedProcFile("/proc/meminfo", "Hugepagesize")
	if err != nil {
		t.Skipf("no Hugepagesize on this host: %v", err)
	}
	_, name, err := detectHugepagesResource()
	if err != nil || name != hugepagesResourceName(fields["Hugepagesize"]) {
		t.Fatalf("override advertises %q, %v; want %q", name, err, hugepagesResourceName(fields["Hugepagesize"]))
	}
}

func TestNodeResourcesDefaults(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires /proc")
	}
	rootDir := CocoonRootDir()
	if _, err := os.Stat(rootDir); err != nil {
		t.Skipf("requires %s for statfs", rootDir)
	}
	capacity, alloc, err := NodeResources()
	if err != nil {
		t.Fatalf("NodeResources: %v", err)
	}

	cpu := capacity.Cpu()
	if cpu.IsZero() {
		t.Errorf("capacity CPU is zero")
	}
	mem := capacity.Memory()
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
