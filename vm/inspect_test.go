package vm

import "testing"

func TestParseInspectJSON(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
  "id": "vm-1",
  "state": "running",
  "config": {
    "name": "demo-vm",
    "cpu": 4,
    "memory": 4294967296
  },
  "network_configs": [
    {
      "mac": "02:00:00:00:00:01",
      "network": {
        "ip": "10.89.0.2"
      }
    }
  ]
}`)

	got, err := parseInspectJSON(raw)
	if err != nil {
		t.Fatalf("parseInspectJSON: %v", err)
	}
	if got.ID != "vm-1" {
		t.Errorf("ID = %q, want %q", got.ID, "vm-1")
	}
	if got.Name != "demo-vm" {
		t.Errorf("Name = %q, want %q", got.Name, "demo-vm")
	}
	if got.State != "running" {
		t.Errorf("State = %q, want %q", got.State, "running")
	}
	if got.CPU != 4 {
		t.Errorf("CPU = %d, want %d", got.CPU, 4)
	}
	if got.Mem != 4294967296 {
		t.Errorf("Mem = %d, want %d", got.Mem, int64(4294967296))
	}
	if got.MAC != "02:00:00:00:00:01" {
		t.Errorf("MAC = %q, want %q", got.MAC, "02:00:00:00:00:01")
	}
	if got.IP != "10.89.0.2" {
		t.Errorf("IP = %q, want %q", got.IP, "10.89.0.2")
	}
}

func TestParseVMListJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     []byte
		wantLen int
	}{
		{
			name:    "empty message",
			raw:     []byte("No VMs found.\n"),
			wantLen: 0,
		},
		{
			name: "json array",
			raw: []byte(`[
  {
    "id": "vm-1",
    "state": "running",
    "config": {"name": "demo-1", "cpu": 2, "memory": 2147483648},
    "network_configs": [{"mac": "02:00:00:00:00:01"}]
  },
  {
    "id": "vm-2",
    "state": "stopped",
    "config": {"name": "demo-2", "cpu": 1, "memory": 1073741824}
  }
]`),
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseVMListJSON(tt.raw)
			if err != nil {
				t.Fatalf("parseVMListJSON: %v", err)
			}
			if len(got) != tt.wantLen {
				t.Fatalf("len(parseVMListJSON()) = %d, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestParseSnapshotJSON(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
  "id": "snap-1",
  "name": "demo-snapshot",
  "image": "https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64.img",
  "hypervisor": "firecracker"
}`)

	got, err := parseSnapshotJSON(raw)
	if err != nil {
		t.Fatalf("parseSnapshotJSON: %v", err)
	}
	if got.ID != "snap-1" {
		t.Fatalf("ID = %q, want snap-1", got.ID)
	}
	if got.Name != "demo-snapshot" {
		t.Fatalf("Name = %q, want demo-snapshot", got.Name)
	}
	if got.Image == "" {
		t.Fatal("Image should not be empty")
	}
	if got.Hypervisor != "firecracker" {
		t.Fatalf("Hypervisor = %q, want firecracker", got.Hypervisor)
	}
}
