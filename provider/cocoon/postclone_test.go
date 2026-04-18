package cocoon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestNeedsPostClone(t *testing.T) {
	t.Parallel()

	staticNIC := []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff", Network: &vm.NetworkInfo{IP: "10.0.0.2", Prefix: 24, Gateway: "10.0.0.1"}}}
	dhcpNIC := []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff"}}

	cases := []struct {
		name        string
		backend     string
		vmID        string
		sourceImage string
		nics        []*vm.NetworkConfig
		want        bool
	}{
		{name: "CH + OCI + DHCP (auto)", backend: "cloud-hypervisor", vmID: "x", nics: dhcpNIC, want: false},
		{name: "CH + OCI + Static", backend: "cloud-hypervisor", vmID: "x", nics: staticNIC, want: true},
		{name: "CH + cloudimg (from sourceImage)", backend: "cloud-hypervisor", vmID: "x", sourceImage: "https://cloud-images.ubuntu.com/img.img", nics: dhcpNIC, want: true},
		{name: "FC + OCI + DHCP", backend: "firecracker", vmID: "x", nics: dhcpNIC, want: true},
		{name: "FC + OCI + Static", backend: "firecracker", vmID: "x", nics: staticNIC, want: true},
		{name: "CH + OCI + no NICs (auto)", backend: "cloud-hypervisor", vmID: "x", nics: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := needsPostClone(tc.backend, tc.vmID, tc.sourceImage, tc.nics)
			if got != tc.want {
				t.Errorf("needsPostClone(%q, %q, %q, %d nics) = %v, want %v", tc.backend, tc.vmID, tc.sourceImage, len(tc.nics), got, tc.want)
			}
		})
	}
}

func TestNeedsPostCloneCloudimgFromDisk(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("COCOON_ROOT_DIR", rootDir)
	vmID := "fake-cloudimg-vm"
	vmDir := filepath.Join(rootDir, "run", "cloudhypervisor", vmID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(vmDir, "overlay.qcow2"), []byte{}, 0o644); err != nil {
		t.Fatalf("write qcow2: %v", err)
	}

	staticNIC := []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff", Network: &vm.NetworkInfo{IP: "10.0.0.2", Prefix: 24, Gateway: "10.0.0.1"}}}
	dhcpNIC := []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff"}}

	// Cloudimg detected from disk (empty sourceImage) — still needs hint
	if !needsPostClone("cloud-hypervisor", vmID, "", staticNIC) {
		t.Errorf("cloudimg + static should need post-clone hint")
	}
	if !needsPostClone("cloud-hypervisor", vmID, "", dhcpNIC) {
		t.Errorf("cloudimg + DHCP should need post-clone hint")
	}
}

func TestBuildPostCloneCommands(t *testing.T) {
	t.Parallel()

	t.Run("FC + static + 2 NICs", func(t *testing.T) {
		nics := []*vm.NetworkConfig{
			{MAC: "aa:bb:cc:00:11:22", Network: &vm.NetworkInfo{IP: "10.0.0.2", Prefix: 24, Gateway: "10.0.0.1"}},
			{MAC: "aa:bb:cc:00:11:33", Network: &vm.NetworkInfo{IP: "10.0.0.3", Prefix: 24, Gateway: "10.0.0.1"}},
		}
		cmds := buildPostCloneCommands("my-vm", vm.BackendFirecracker, "x", "", nics)

		if !strings.Contains(cmds, "ip link set dev eth0") {
			t.Errorf("missing MAC fixup for eth0")
		}
		if !strings.Contains(cmds, "ip link set dev eth1") {
			t.Errorf("missing MAC fixup for eth1")
		}
		if !strings.Contains(cmds, "Address=10.0.0.2/24") {
			t.Errorf("missing static config for NIC 0")
		}
	})

	t.Run("CH + DHCP + 1 NIC", func(t *testing.T) {
		nics := []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff"}}
		cmds := buildPostCloneCommands("clone-vm", "cloud-hypervisor", "x", "", nics)

		if strings.Contains(cmds, "ip link set") {
			t.Errorf("CH should not have MAC fixup")
		}
		if !strings.Contains(cmds, "DHCP=ipv4") {
			t.Errorf("missing DHCP config")
		}
	})

	t.Run("CH + cloudimg (from sourceImage)", func(t *testing.T) {
		nics := []*vm.NetworkConfig{{MAC: "aa:bb:cc:dd:ee:ff"}}
		cmds := buildPostCloneCommands("ci-vm", "cloud-hypervisor", "x", "https://cloud-images.ubuntu.com/img.img", nics)

		if !strings.Contains(cmds, "cloud-init clean") {
			t.Errorf("cloudimg should use cloud-init clean")
		}
		if !strings.Contains(cmds, "cloud-init init") {
			t.Errorf("cloudimg should use cloud-init init")
		}
		if strings.Contains(cmds, "printf") {
			t.Errorf("cloudimg should not write networkd files directly")
		}
	})

	t.Run("FC + mixed static/DHCP", func(t *testing.T) {
		nics := []*vm.NetworkConfig{
			{MAC: "aa:00:00:00:00:01", Network: &vm.NetworkInfo{IP: "10.0.0.5", Prefix: 16, Gateway: "10.0.0.1"}},
			{MAC: "aa:00:00:00:00:02"},
		}
		cmds := buildPostCloneCommands("mixed", vm.BackendFirecracker, "x", "", nics)

		if !strings.Contains(cmds, "Address=10.0.0.5/16") {
			t.Errorf("missing static config")
		}
		if !strings.Contains(cmds, "DHCP=ipv4") {
			t.Errorf("missing DHCP config for second NIC")
		}
	})
}
