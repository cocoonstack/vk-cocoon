// Package network resolves VM IPs from cocoon-net's JSON lease file.
package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultLeasesPath is cocoon-net's default JSON lease file location.
	DefaultLeasesPath = "/var/lib/cocoon/net/leases.json"
)

// ErrNoLease means no lease matches the lookup.
var (
	ErrNoLease = errors.New("no cocoon-net lease for the requested MAC")
)

// Lease is one cocoon-net DHCP entry.
type Lease struct {
	MAC     string
	IP      string
	Expires time.Time
}

// LeaseParser reads cocoon-net leases, caching until mtime changes.
type LeaseParser struct {
	Path string

	mu     sync.Mutex
	mtime  time.Time
	size   int64
	cached []Lease
	byMAC  map[string]*Lease
}

// cocoonNetLease is the on-disk JSON shape written by cocoon-net.
type cocoonNetLease struct {
	MAC    string `json:"mac"`
	IP     string `json:"ip"`
	Expiry string `json:"expiry"`
}

// NewLeaseParser returns a parser; empty path uses the default.
func NewLeaseParser(path string) *LeaseParser {
	if path == "" {
		path = DefaultLeasesPath
	}
	return &LeaseParser{Path: path}
}

// LookupByMAC returns the lease matching mac (case-insensitive).
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

// All returns every lease in the file.
func (p *LeaseParser) All() ([]Lease, error) {
	if err := p.refresh(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.cached), nil
}

// refresh re-reads the lease file when mtime or size changed.
func (p *LeaseParser) refresh() error {
	info, err := os.Stat(p.Path)
	if err != nil {
		return fmt.Errorf("stat lease file %s: %w", p.Path, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached != nil && info.ModTime().Equal(p.mtime) && info.Size() == p.size {
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
	p.cached = leases
	p.byMAC = byMAC
	p.mtime = info.ModTime()
	p.size = info.Size()
	return nil
}

// parse decodes cocoon-net's JSON lease file.
func (p *LeaseParser) parse() ([]Lease, error) {
	data, err := os.ReadFile(p.Path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("read lease file %s: %w", p.Path, err)
	}
	var raw []cocoonNetLease
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode leases %s: %w", p.Path, err)
	}
	out := make([]Lease, 0, len(raw))
	for _, r := range raw {
		// Tolerate partial rows: cocoon-net may flush a lease mid-write,
		// and one bad timestamp shouldn't block lookups for healthy leases.
		expiry, err := time.Parse(time.RFC3339, r.Expiry)
		if err != nil {
			continue
		}
		out = append(out, Lease{
			MAC:     r.MAC,
			IP:      r.IP,
			Expires: expiry,
		})
	}
	return out, nil
}
