package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// Pinger reports whether an IP is reachable over ICMPv4 echo.
type Pinger interface {
	Ping(ctx context.Context, ip string) error
}

// NopPinger always succeeds; used when CAP_NET_RAW is unavailable.
type NopPinger struct{}

// Ping always returns nil; NopPinger is a no-op fallback.
func (NopPinger) Ping(_ context.Context, _ string) error { return nil }

// ICMPPinger opens a fresh raw ICMPv4 socket per Ping call for concurrency safety.
type ICMPPinger struct {
	id int // ICMP identifier, fixed per process
}

// NewICMPPinger probes for CAP_NET_RAW and returns a Pinger, or an error
// indicating the caller should fall back to NopPinger.
func NewICMPPinger() (*ICMPPinger, error) {
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("open icmp socket: %w", err)
	}
	// Capability-check only; fresh socket per Ping.
	if closeErr := conn.Close(); closeErr != nil {
		return nil, fmt.Errorf("close probe socket: %w", closeErr)
	}
	return &ICMPPinger{id: os.Getpid() & 0xffff}, nil
}

// Ping sends a single ICMPv4 echo and waits for the reply within ctx's deadline.
func (p *ICMPPinger) Ping(ctx context.Context, ip string) error {
	addr := &net.IPAddr{IP: net.ParseIP(ip)}
	if addr.IP == nil {
		return fmt.Errorf("invalid ip %q", ip)
	}
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return fmt.Errorf("open icmp socket: %w", err)
	}
	defer func() { _ = conn.Close() }()

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   p.id,
			Seq:  1,
			Data: []byte("vk-cocoon"),
		},
	}
	b, err := msg.Marshal(nil)
	if err != nil {
		return fmt.Errorf("marshal icmp echo: %w", err)
	}
	// Refuse unbounded waits; callers must set a deadline.
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("icmp ping requires context deadline")
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}
	if _, err := conn.WriteTo(b, addr); err != nil {
		return fmt.Errorf("send icmp echo to %s: %w", ip, err)
	}

	reply := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(reply)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("read icmp reply: %w", err)
		}
		parsed, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), reply[:n])
		if err != nil {
			continue
		}
		if parsed.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		// Also match on peer address to avoid cross-socket reply stealing.
		echo, ok := parsed.Body.(*icmp.Echo)
		if !ok || echo.ID != p.id {
			continue
		}
		if peerAddr, ok := peer.(*net.IPAddr); ok && !peerAddr.IP.Equal(addr.IP) {
			continue
		}
		return nil
	}
}
