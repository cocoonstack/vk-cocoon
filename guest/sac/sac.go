// Package sac implements the guest.Dialer/Session interfaces for
// Windows SAC (Special Administration Console) over a serial console
// Unix socket. A single Dial opens a persistent connection; multiple
// Run calls reuse that connection so the serial port state is preserved.
package sac

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
	"github.com/cocoonstack/vk-cocoon/guest"
)

const (
	defaultWaitReady  = 60 * time.Second
	defaultRWTimeout  = 5 * time.Second
	defaultCmdTimeout = 5 * time.Second
	retryInterval     = 2 * time.Second
	readBufSize       = 4096
	sacPrompt         = "SAC>"
)

// netLineRe matches a SAC "i" output line announcing a NIC IP.
var netLineRe = regexp.MustCompile(`Net:\s+(\d+),\s+Ip=(\S+)`)

var _ guest.Dialer = (*Dialer)(nil)

// Dialer opens persistent SAC sessions over serial console sockets.
type Dialer struct {
	WaitReady time.Duration // total Dial budget: connect + SAC> prompt; 0 = 60s
}

func (d *Dialer) Dial(ctx context.Context, target string) (guest.Session, error) {
	deadline := time.Now().Add(d.waitReady())
	conn, err := dial(ctx, target, deadline)
	if err != nil {
		return nil, err
	}
	if promptErr := waitPrompt(ctx, conn, time.Until(deadline)); promptErr != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sac wait prompt: %w", promptErr)
	}
	return &Session{conn: conn}, nil
}

func (d *Dialer) waitReady() time.Duration {
	if d.WaitReady > 0 {
		return d.WaitReady
	}
	return defaultWaitReady
}

var _ guest.Session = (*Session)(nil)

// Session is a persistent SAC console connection. Commands are sent
// sequentially over the same serial socket.
type Session struct {
	conn net.Conn
}

func (s *Session) Run(ctx context.Context, cmd []string, stdout io.Writer) error {
	output, err := sacCommand(ctx, s.conn, strings.Join(cmd, " "), defaultCmdTimeout)
	if err != nil {
		return err
	}
	if stdout != nil {
		_, _ = io.WriteString(stdout, output)
	}
	return nil
}

func (s *Session) Close() error {
	return s.conn.Close()
}

// ParseNetEntries extracts deduplicated net numbers from SAC "i"
// command output, preserving enumeration order. SAC lists both IPv4
// and IPv6 lines for each NIC; this function deduplicates by number.
func ParseNetEntries(output string) []int {
	var nums []int
	seen := map[int]bool{}
	for _, match := range netLineRe.FindAllStringSubmatch(output, -1) {
		num, _ := strconv.Atoi(match[1])
		if seen[num] {
			continue
		}
		seen[num] = true
		nums = append(nums, num)
	}
	return nums
}

// NetHasIP reports whether the SAC "i" output contains the given IP
// for the specified net number.
func NetHasIP(output string, netNum int, ip string) bool {
	return slices.ContainsFunc(netLineRe.FindAllStringSubmatch(output, -1), func(match []string) bool {
		num, _ := strconv.Atoi(match[1])
		return num == netNum && match[2] == ip
	})
}

func dial(ctx context.Context, socketPath string, deadline time.Time) (net.Conn, error) {
	var lastErr error
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return nil, fmt.Errorf("sac dial timeout: %w", lastErr)
			}
			return nil, errors.New("sac dial timeout")
		}
		conn, err := net.DialTimeout("unix", socketPath, defaultRWTimeout)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if !commonk8s.SleepCtx(ctx, retryInterval) {
			return nil, ctx.Err()
		}
	}
}

func waitPrompt(ctx context.Context, conn net.Conn, timeout time.Duration) error {
	if _, err := readUntilPrompt(ctx, conn, timeout, "\r\n"); err != nil {
		return fmt.Errorf("await SAC prompt: %w", err)
	}
	return nil
}

func sacCommand(ctx context.Context, conn net.Conn, cmd string, timeout time.Duration) (string, error) {
	drain(conn)

	_ = conn.SetWriteDeadline(time.Now().Add(defaultRWTimeout))
	if _, err := conn.Write([]byte(cmd + "\r\n")); err != nil {
		return "", fmt.Errorf("write command %q: %w", cmd, err)
	}
	out, err := readUntilPrompt(ctx, conn, timeout, "")
	if err != nil {
		return out, fmt.Errorf("response to %q: %w", cmd, err)
	}
	return out, nil
}

// A non-empty probe is re-sent each iteration to wake a quiet console.
func readUntilPrompt(ctx context.Context, conn net.Conn, timeout time.Duration, probe string) (string, error) {
	var sb strings.Builder
	buf := make([]byte, readBufSize)
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return sb.String(), err
		}
		if time.Now().After(deadline) {
			return sb.String(), errors.New("timeout waiting for SAC> prompt")
		}
		if probe != "" {
			_ = conn.SetWriteDeadline(time.Now().Add(defaultRWTimeout))
			if _, err := conn.Write([]byte(probe)); err != nil {
				return sb.String(), fmt.Errorf("write prompt probe: %w", err)
			}
		}
		_ = conn.SetReadDeadline(time.Now().Add(defaultRWTimeout))
		n, err := conn.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
			if strings.Contains(sb.String(), sacPrompt) {
				return sb.String(), nil
			}
		}
		if err != nil {
			if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
				continue
			}
			return sb.String(), fmt.Errorf("read: %w", err)
		}
	}
}

func drain(conn net.Conn) {
	buf := make([]byte, readBufSize)
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	for {
		_, err := conn.Read(buf)
		if err != nil {
			return
		}
	}
}
