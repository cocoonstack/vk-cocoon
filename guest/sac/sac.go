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
	"strconv"
	"strings"
	"time"

	"github.com/cocoonstack/vk-cocoon/guest"
)

// SAC serial-console timing and buffer defaults.
const (
	defaultWaitReady  = 60 * time.Second
	defaultRWTimeout  = 5 * time.Second
	defaultCmdTimeout = 5 * time.Second
	retryInterval     = 2 * time.Second
	readBufSize       = 4096
	sacPrompt         = "SAC>"
)

// compile-time interface checks and package-level helpers.
var (
	_ guest.Dialer  = (*Dialer)(nil)
	_ guest.Session = (*Session)(nil)

	netLineRe = regexp.MustCompile(`Net:\s+(\d+),\s+Ip=(\S+)`)
)

// Dialer opens persistent SAC sessions over serial console sockets.
type Dialer struct {
	WaitReady time.Duration // max time waiting for SAC> prompt; 0 = 60s
}

// Dial connects to the SAC console at target (a Unix socket path),
// waits for the SAC> prompt, and returns a ready Session.
func (d *Dialer) Dial(ctx context.Context, target string) (guest.Session, error) {
	conn, err := d.dial(ctx, target)
	if err != nil {
		return nil, err
	}
	if promptErr := waitPrompt(conn, d.waitReady()); promptErr != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sac wait prompt: %w", promptErr)
	}
	return &Session{conn: conn}, nil
}

// Session is a persistent SAC console connection. Commands are sent
// sequentially over the same serial socket.
type Session struct {
	conn net.Conn
}

// Run sends a SAC command and writes the response to stdout.
func (s *Session) Run(_ context.Context, cmd []string, stdout io.Writer) error {
	output, err := sacCommand(s.conn, strings.Join(cmd, " "), defaultCmdTimeout)
	if err != nil {
		return err
	}
	if stdout != nil {
		_, _ = io.WriteString(stdout, output)
	}
	return nil
}

// Close releases the underlying socket.
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
	for _, match := range netLineRe.FindAllStringSubmatch(output, -1) {
		num, _ := strconv.Atoi(match[1])
		if num == netNum && match[2] == ip {
			return true
		}
	}
	return false
}

func (d *Dialer) waitReady() time.Duration {
	if d.WaitReady > 0 {
		return d.WaitReady
	}
	return defaultWaitReady
}

func (d *Dialer) dial(ctx context.Context, socketPath string) (net.Conn, error) {
	deadline := time.Now().Add(d.waitReady())
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
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryInterval):
		}
	}
}

// waitPrompt sends CR+LF and waits until the SAC> prompt appears.
func waitPrompt(conn net.Conn, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	buf := make([]byte, readBufSize)
	for {
		if time.Now().After(deadline) {
			return errors.New("timeout waiting for SAC> prompt")
		}
		_ = conn.SetWriteDeadline(time.Now().Add(defaultRWTimeout))
		if _, err := conn.Write([]byte("\r\n")); err != nil {
			return fmt.Errorf("write prompt probe: %w", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(defaultRWTimeout))
		n, err := conn.Read(buf)
		if n > 0 && strings.Contains(string(buf[:n]), sacPrompt) {
			return nil
		}
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return fmt.Errorf("read prompt: %w", err)
		}
	}
}

// sacCommand sends a command and reads the response until SAC> appears.
func sacCommand(conn net.Conn, cmd string, timeout time.Duration) (string, error) {
	drain(conn)

	_ = conn.SetWriteDeadline(time.Now().Add(defaultRWTimeout))
	if _, err := conn.Write([]byte(cmd + "\r\n")); err != nil {
		return "", fmt.Errorf("write command %q: %w", cmd, err)
	}

	var sb strings.Builder
	buf := make([]byte, readBufSize)
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return sb.String(), fmt.Errorf("timeout waiting for response to %q", cmd)
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
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return sb.String(), fmt.Errorf("read response: %w", err)
		}
	}
}

// drain reads and discards any pending data on the connection.
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
