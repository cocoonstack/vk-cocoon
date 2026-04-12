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

// Pinger reports whether an IP is reachable over ICMPv4 echo. The
// probes package uses this to decide when a freshly booted guest is
// actually serving — weaker than "SSH/RDP is up" and therefore
// usable across Linux and Windows images (the cocoon Windows golden
// image's autounattend.xml explicitly opens ICMPv4 echo and disables
// all firewall profiles, so this probe matches that contract).
type Pinger interface {
	// Ping sends one ICMP echo request to ip and returns nil if a
	// reply is received before the context deadline.
	Ping(ctx context.Context, ip string) error
}

// NopPinger is the fallback Pinger used when the host does not grant
// CAP_NET_RAW (or any other error prevents opening a raw socket).
// Every call returns nil, which makes readiness degrade to "an IP
// was resolved" — still strictly better than the old behavior of
// marking the pod ready before any network check.
type NopPinger struct{}

// Ping always reports success.
func (NopPinger) Ping(_ context.Context, _ string) error { return nil }

// ICMPPinger is the production Pinger. Each Ping call opens its own
// short-lived raw ICMPv4 socket so concurrent probes to different
// targets cannot steal each other's replies — a shared-socket
// design would force us to either serialize every Ping on a global
// mutex or build a dispatcher goroutine that demultiplexes replies
// by sequence number, both of which are more machinery than a
// handful of microsecond socket opens per pod.
type ICMPPinger struct {
	// id is the ICMP identifier field, fixed per process so the
	// kernel's IP-layer filter still routes replies back to us
	// when the raw socket is the one it happens to schedule.
	id int
}

// NewICMPPinger probes whether the current process can open a raw
// ICMPv4 socket (typically meaning CAP_NET_RAW is granted) and
// returns a ready-to-use Pinger. Callers that get an error should
// fall back to NopPinger.
func NewICMPPinger() (*ICMPPinger, error) {
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, fmt.Errorf("open icmp socket: %w", err)
	}
	// Capability-check only: we reopen a fresh socket per Ping so
	// there is no long-lived connection to hand out.
	if closeErr := conn.Close(); closeErr != nil {
		return nil, fmt.Errorf("close probe socket: %w", closeErr)
	}
	return &ICMPPinger{id: os.Getpid() & 0xffff}, nil
}

// Ping sends a single ICMPv4 echo to ip and waits for the matching
// reply. The context bounds the wait; a canceled or expired
// context surfaces as ctx.Err(). Each call owns its own socket, so
// concurrent Pings to different targets are fully independent.
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
	// Callers (the probes package) always wrap Ping in a
	// WithTimeout; refuse the unbounded case so a future caller
	// forgetting to do so fails loudly instead of hanging on the
	// raw socket read.
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
			if ctx.Err() != nil {
				return ctx.Err()
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
		// The per-call socket would normally demultiplex for us,
		// but the kernel still delivers every echo reply that
		// matches icmp.id == p.id across every socket. Match on
		// the peer address too so we never surface someone else's
		// reply as ours.
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
