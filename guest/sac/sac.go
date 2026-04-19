// Package sac implements the guest.Executor interface for Windows SAC
// (Special Administration Console) over a serial console Unix socket.
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

const (
	defaultWaitReady = 60 * time.Second
	defaultRWTimeout = 5 * time.Second
	retryInterval    = 2 * time.Second
	readBufSize      = 4096
	sacPrompt        = "SAC>"
)

var (
	// compile-time interface check.
	_ guest.Executor = (*Executor)(nil)

	netLineRe = regexp.MustCompile(`Net:\s+(\d+),\s+Ip=(\S+)`)
)

// Executor sends commands to Windows SAC via a serial console Unix socket.
// Each Run call opens a fresh connection, waits for the SAC> prompt,
// sends the command, and streams the response to stdout.
type Executor struct {
	WaitReady time.Duration // max time waiting for SAC> prompt; 0 = 60s
}

// Run connects to the SAC console at target (a Unix socket path),
// sends strings.Join(cmd, " ") as a single SAC command, and writes
// the response to stdout. stdin and stderr are unused.
func (e *Executor) Run(ctx context.Context, target string, cmd []string, _ io.Reader, stdout, _ io.Writer) error {
	conn, err := e.dial(ctx, target)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if promptErr := waitPrompt(conn, e.waitReady()); promptErr != nil {
		return fmt.Errorf("sac wait prompt: %w", promptErr)
	}

	output, err := sacCommand(conn, strings.Join(cmd, " "), 3*time.Second)
	if err != nil {
		return err
	}
	if stdout != nil {
		_, _ = io.WriteString(stdout, output)
	}
	return nil
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

func (e *Executor) waitReady() time.Duration {
	if e.WaitReady > 0 {
		return e.WaitReady
	}
	return defaultWaitReady
}

func (e *Executor) dial(ctx context.Context, socketPath string) (net.Conn, error) {
	deadline := time.Now().Add(e.waitReady())
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
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
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
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
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
