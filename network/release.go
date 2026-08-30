package network

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultControlSocket is cocoon-net's root-only local lifecycle API.
	DefaultControlSocket  = "/run/cocoon-net/control.sock"
	controlRequestTimeout = 5 * time.Second
)

// LeaseReleaser frees a cocoon-net DHCP lease by guest MAC; implementations bound their own request time.
type LeaseReleaser interface {
	ReleaseByMAC(context.Context, string) error
}

// CocoonNetLeaseReleaser talks to cocoon-net over its local Unix socket.
type CocoonNetLeaseReleaser struct {
	client *http.Client
}

// NewCocoonNetLeaseReleaser constructs a reusable local control client.
func NewCocoonNetLeaseReleaser(socketPath string) *CocoonNetLeaseReleaser {
	dialer := &net.Dialer{Timeout: controlRequestTimeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &CocoonNetLeaseReleaser{
		client: &http.Client{Transport: transport, Timeout: controlRequestTimeout},
	}
}

// ReleaseByMAC idempotently releases a lease; cocoon-net returns 204 whether the lease existed or had already been reclaimed.
func (r *CocoonNetLeaseReleaser) ReleaseByMAC(ctx context.Context, rawMAC string) error {
	mac, err := net.ParseMAC(rawMAC)
	if err != nil {
		return fmt.Errorf("parse MAC %q: %w", rawMAC, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		"http://cocoon-net/v1/leases/"+mac.String(), nil)
	if err != nil {
		return fmt.Errorf("build lease release request: %w", err)
	}
	var lastErr error
	for attempt := range 2 {
		resp, err := r.client.Do(req)
		if err != nil {
			return fmt.Errorf("release lease for %s: %w", mac, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		lastErr = fmt.Errorf("release lease for %s: cocoon-net returned %s: %s",
			mac, resp.Status, strings.TrimSpace(string(body)))
		if resp.StatusCode != http.StatusInternalServerError || attempt > 0 {
			return lastErr
		}
	}
	return lastErr
}
