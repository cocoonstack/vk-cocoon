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

// newTestParser writes data to a temp leases.json and returns a parser for it.
func newTestParser(t *testing.T, data string) *LeaseParser {
	t.Helper()
	path := filepath.Join(t.TempDir(), "leases.json")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write leases: %v", err)
	}
	return NewLeaseParser(path)
}

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

func TestLeaseParserAll(t *testing.T) {
	t.Parallel()
	p := newTestParser(t, twoLeases)
	leases, err := p.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(leases) != 2 {
		t.Fatalf("len = %d, want 2", len(leases))
	}
}

func TestLeaseParserSkipsMalformedExpiry(t *testing.T) {
	t.Parallel()
	data := `[
  {"mac":"aa:bb:cc:dd:ee:ff","ip":"172.20.0.10","expiry":"not-a-timestamp"},
  {"mac":"11:22:33:44:55:66","ip":"172.20.0.11","expiry":"2099-01-01T00:00:00Z"}
]`
	p := newTestParser(t, data)
	leases, err := p.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("malformed entry should be skipped, got %d leases", len(leases))
	}
}
