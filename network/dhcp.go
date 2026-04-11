// Package network resolves IP addresses for cocoon VMs by parsing
// the dnsmasq lease file the host runs alongside the cocoon bridge.
package network

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// defaultLeasesPath is the standard dnsmasq lease file path on
	// the cocoon hosts that cocoon-net provisions.
	defaultLeasesPath = "/var/lib/dnsmasq/dnsmasq.leases"
)

// ErrNoLease is returned when no lease matches the lookup.
var ErrNoLease = errors.New("no dnsmasq lease for the requested MAC")

// Lease describes one DHCP entry the parser found.
type Lease struct {
	Expires  time.Time
	MAC      string
	IP       string
	Hostname string
}

// LeaseParser reads dnsmasq leases from a file, caching the parsed
// result until the lease file's mtime changes. GetPodStatus calls
// this on every kubelet heartbeat, so re-parsing unconditionally
// would be a hot-path waste.
type LeaseParser struct {
	Path string

	mu     sync.Mutex
	mtime  time.Time
	size   int64
	cached []Lease
	byMAC  map[string]*Lease
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
	if err := p.refresh(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if lease, ok := p.byMAC[strings.ToLower(mac)]; ok {
		return lease, nil
	}
	return nil, ErrNoLease
}

// All returns every lease currently in the file.
func (p *LeaseParser) All() ([]Lease, error) {
	if err := p.refresh(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Lease, len(p.cached))
	copy(out, p.cached)
	return out, nil
}

// refresh re-reads the lease file when mtime or size has changed.
// When the file is unchanged the cached slice is reused verbatim.
func (p *LeaseParser) refresh() error {
	info, err := os.Stat(p.Path)
	if err != nil {
		return fmt.Errorf("stat lease file %s: %w", p.Path, err)
	}
	p.mu.Lock()
	fresh := p.cached != nil && info.ModTime().Equal(p.mtime) && info.Size() == p.size
	p.mu.Unlock()
	if fresh {
		return nil
	}

	leases, err := p.parse()
	if err != nil {
		return err
	}
	byMAC := make(map[string]*Lease, len(leases))
	for i := range leases {
		byMAC[strings.ToLower(leases[i].MAC)] = &leases[i]
	}
	p.mu.Lock()
	p.cached = leases
	p.byMAC = byMAC
	p.mtime = info.ModTime()
	p.size = info.Size()
	p.mu.Unlock()
	return nil
}

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
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0), nil
}
