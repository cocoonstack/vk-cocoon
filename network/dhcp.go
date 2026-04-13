// Package network resolves VM IPs from the host's dnsmasq lease file.
package network

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultLeasesPath = "/var/lib/misc/dnsmasq.leases"
)

// ErrNoLease means no lease matches the lookup.
var ErrNoLease = errors.New("no dnsmasq lease for the requested MAC")

// Lease is one dnsmasq DHCP entry.
type Lease struct {
	Expires  time.Time
	MAC      string
	IP       string
	Hostname string
}

// LeaseParser reads dnsmasq leases, caching until mtime changes.
type LeaseParser struct {
	Path string

	mu     sync.Mutex
	mtime  time.Time
	size   int64
	cached []Lease
	byMAC  map[string]*Lease
}

// NewLeaseParser returns a parser; empty path uses the default.
func NewLeaseParser(path string) *LeaseParser {
	if path == "" {
		path = defaultLeasesPath
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

// parse reads the lease file. Format: <expiry> <mac> <ip> <hostname> <client-id>
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
		expiry, err := parseUnixSecond(fields[0])
		if err != nil {
			// Skip lines with malformed timestamps.
			continue
		}
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
