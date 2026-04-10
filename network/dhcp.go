// Package network resolves IP addresses for cocoon VMs by parsing
// the dnsmasq lease file the host runs alongside the cocoon bridge.
package network

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// defaultLeasesPath is the standard dnsmasq lease file path on
	// the cocoon hosts that cocoon-net provisions.
	defaultLeasesPath = "/var/lib/dnsmasq/dnsmasq.leases"
)

// Lease describes one DHCP entry the parser found.
type Lease struct {
	Expires  time.Time
	MAC      string
	IP       string
	Hostname string
}

// LeaseParser reads dnsmasq leases from a file. The file path is
// configurable so tests can point at fixtures.
type LeaseParser struct {
	Path string
}

// NewLeaseParser returns a parser that reads from path. Empty path
// resolves to the standard /var/lib/dnsmasq/dnsmasq.leases.
func NewLeaseParser(path string) *LeaseParser {
	if path == "" {
		path = defaultLeasesPath
	}
	return &LeaseParser{Path: path}
}

// LookupByMAC returns the first lease whose MAC matches mac
// (case-insensitive). Returns ErrNoLease when no lease matches.
func (p *LeaseParser) LookupByMAC(mac string) (*Lease, error) {
	leases, err := p.parse()
	if err != nil {
		return nil, err
	}
	mac = strings.ToLower(mac)
	for i := range leases {
		if strings.ToLower(leases[i].MAC) == mac {
			return &leases[i], nil
		}
	}
	return nil, ErrNoLease
}

// All returns every lease currently in the file.
func (p *LeaseParser) All() ([]Lease, error) {
	return p.parse()
}

// ErrNoLease is returned when no lease matches the lookup.
var ErrNoLease = errors.New("no dnsmasq lease for the requested MAC")

// parse reads the lease file and returns a slice of Lease records.
// dnsmasq leases are space-separated:
//
//	<expiry> <mac> <ip> <hostname> <client-id>
func (p *LeaseParser) parse() ([]Lease, error) {
	f, err := os.Open(p.Path)
	if err != nil {
		return nil, fmt.Errorf("open lease file %s: %w", p.Path, err)
	}
	defer func() { _ = f.Close() }()

	var out []Lease
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		expiry, _ := parseUnixSecond(fields[0])
		out = append(out, Lease{
			Expires:  expiry,
			MAC:      fields[1],
			IP:       fields[2],
			Hostname: fields[3],
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan lease file: %w", err)
	}
	return out, nil
}

func parseUnixSecond(s string) (time.Time, error) {
	var sec int64
	if _, err := fmt.Sscanf(s, "%d", &sec); err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0), nil
}
