package network

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLeaseParserLookupByMAC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leases.json")
	data := `[
  {"mac":"aa:bb:cc:dd:ee:ff","ip":"172.20.0.10","expiry":"2099-01-01T00:00:00Z"},
  {"mac":"11:22:33:44:55:66","ip":"172.20.0.11","expiry":"2099-01-01T00:00:00Z"}
]`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	p := NewLeaseParser(path)
	lease, err := p.LookupByMAC("aa:bb:cc:dd:ee:ff")
	if err != nil {
		t.Fatalf("LookupByMAC: %v", err)
	}
	if lease.IP != "172.20.0.10" {
		t.Errorf("IP = %q, want 172.20.0.10", lease.IP)
	}
	// Case-insensitive lookup.
	if _, err := p.LookupByMAC("AA:BB:CC:DD:EE:FF"); err != nil {
		t.Errorf("uppercase MAC lookup failed: %v", err)
	}
	// Missing MAC returns ErrNoLease.
	if _, err := p.LookupByMAC("99:99:99:99:99:99"); err != ErrNoLease {
		t.Errorf("missing MAC err = %v, want ErrNoLease", err)
	}
}

func TestLeaseParserAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leases.json")
	data := `[
  {"mac":"aa:bb:cc:dd:ee:ff","ip":"172.20.0.10","expiry":"2099-01-01T00:00:00Z"},
  {"mac":"11:22:33:44:55:66","ip":"172.20.0.11","expiry":"2099-01-01T00:00:00Z"}
]`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	p := NewLeaseParser(path)
	leases, err := p.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(leases) != 2 {
		t.Fatalf("len = %d, want 2", len(leases))
	}
}

func TestLeaseParserSkipsMalformedExpiry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leases.json")
	data := `[
  {"mac":"aa:bb:cc:dd:ee:ff","ip":"172.20.0.10","expiry":"not-a-timestamp"},
  {"mac":"11:22:33:44:55:66","ip":"172.20.0.11","expiry":"2099-01-01T00:00:00Z"}
]`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	p := NewLeaseParser(path)
	leases, err := p.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("malformed entry should be skipped, got %d leases", len(leases))
	}
}
