package network

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const twoLeases = `[
  {"mac":"aa:bb:cc:dd:ee:ff","ip":"172.20.0.10","expiry":"2099-01-01T00:00:00Z"},
  {"mac":"11:22:33:44:55:66","ip":"172.20.0.11","expiry":"2099-01-01T00:00:00Z"}
]`

func TestLeaseParserLookupByMAC(t *testing.T) {
	t.Parallel()
	p := newTestParser(t, twoLeases)

	tests := []struct {
		name    string
		mac     string
		wantIP  string
		wantErr error
	}{
		{"found", "aa:bb:cc:dd:ee:ff", "172.20.0.10", nil},
		{"case-insensitive", "AA:BB:CC:DD:EE:FF", "172.20.0.10", nil},
		{"missing", "99:99:99:99:99:99", "", ErrNoLease},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lease, err := p.LookupByMAC(tt.mac)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && lease.IP != tt.wantIP {
				t.Errorf("IP = %q, want %q", lease.IP, tt.wantIP)
			}
		})
	}
}

func TestLeaseParserSkipsMalformedExpiry(t *testing.T) {
	t.Parallel()
	data := `[
  {"mac":"aa:bb:cc:dd:ee:ff","ip":"172.20.0.10","expiry":"not-a-timestamp"},
  {"mac":"11:22:33:44:55:66","ip":"172.20.0.11","expiry":"2099-01-01T00:00:00Z"}
]`
	p := newTestParser(t, data)
	if _, err := p.LookupByMAC("aa:bb:cc:dd:ee:ff"); !errors.Is(err, ErrNoLease) {
		t.Fatalf("malformed entry should be skipped, got err %v", err)
	}
	lease, err := p.LookupByMAC("11:22:33:44:55:66")
	if err != nil {
		t.Fatalf("LookupByMAC: %v", err)
	}
	if lease.IP != "172.20.0.11" {
		t.Errorf("IP = %q, want %q", lease.IP, "172.20.0.11")
	}
}

// newTestParser writes data to a temp leases.json and returns a parser for it.
func newTestParser(t *testing.T, data string) *LeaseParser {
	t.Helper()
	path := filepath.Join(t.TempDir(), "leases.json")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write leases: %v", err)
	}
	return NewLeaseParser(path)
}
