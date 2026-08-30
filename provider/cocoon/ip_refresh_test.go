package cocoon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/vk-cocoon/network"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestResolveVMIPReplacesStaleCachedIP(t *testing.T) {
	p := newTestProvider(t)
	p.LeaseParser = newLeaseParser(t, "aa:bb:cc:dd:ee:ff", "172.20.0.88")

	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.trackPod(pod, &vm.VM{
		ID:   "vmid",
		Name: "vk-ns-demo-0",
		MAC:  "aa:bb:cc:dd:ee:ff",
		IP:   "172.20.0.42",
	})

	tracked := p.vmForPod("ns", "demo-0")
	if got := p.resolveVMIP("ns", "demo-0", tracked); got != "172.20.0.88" {
		t.Fatalf("resolveVMIP = %q, want renewed lease 172.20.0.88", got)
	}
	if got := p.vmForPod("ns", "demo-0").IP; got != "172.20.0.88" {
		t.Fatalf("tracked VM IP = %q, want renewed lease 172.20.0.88", got)
	}
}

func TestResolveVMIPKeepsStaticAddress(t *testing.T) {
	p := newTestProvider(t)
	p.LeaseParser = newLeaseParser(t, "aa:bb:cc:dd:ee:ff", "172.20.0.88")

	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.trackPod(pod, &vm.VM{
		ID:   "vmid",
		Name: "vk-ns-demo-0",
		MAC:  "aa:bb:cc:dd:ee:ff",
		IP:   "10.0.0.42",
		NetworkConfigs: []*vm.NetworkConfig{{
			MAC: "aa:bb:cc:dd:ee:ff",
			Network: &vm.NetworkInfo{
				IP: "10.0.0.42",
			},
		}},
	})

	tracked := p.vmForPod("ns", "demo-0")
	if got := p.resolveVMIP("ns", "demo-0", tracked); got != "10.0.0.42" {
		t.Fatalf("resolveVMIP = %q, want static address 10.0.0.42", got)
	}
	if got := p.vmForPod("ns", "demo-0").IP; got != "10.0.0.42" {
		t.Fatalf("tracked VM IP = %q, want unchanged static address 10.0.0.42", got)
	}
}

func TestResolveVMIPClearsExpiredDHCPAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.json")
	if err := os.WriteFile(path, []byte(`[{"mac":"aa:bb:cc:dd:ee:ff","ip":"172.20.0.42","expiry":"2000-01-01T00:00:00Z"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newTestProvider(t)
	p.LeaseParser = network.NewLeaseParser(path)

	pod := newPodWithSpec(meta.VMSpec{VMName: "vk-ns-demo-0", Mode: "run"})
	p.trackPod(pod, &vm.VM{
		ID:   "vmid",
		Name: "vk-ns-demo-0",
		MAC:  "aa:bb:cc:dd:ee:ff",
		IP:   "172.20.0.42",
	})

	tracked := p.vmForPod("ns", "demo-0")
	if got := p.resolveVMIP("ns", "demo-0", tracked); got != "" {
		t.Fatalf("resolveVMIP = %q, want no address after lease expiry", got)
	}
	if got := p.vmForPod("ns", "demo-0").IP; got != "" {
		t.Fatalf("tracked VM IP = %q, want expired cache cleared", got)
	}
}
