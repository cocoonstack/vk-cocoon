package sac

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeSAC simulates a Windows SAC console over a Unix socket.
type fakeSAC struct {
	listener net.Listener
	ips      map[int]string // net# → current IP
}

func newFakeSAC(t *testing.T, socketPath string, netNums []int) *fakeSAC {
	t.Helper()
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ips := map[int]string{}
	for _, n := range netNums {
		ips[n] = "169.254.0.1"
	}
	f := &fakeSAC{listener: ln, ips: ips}
	go f.serve()
	return f
}

func (f *fakeSAC) close() { _ = f.listener.Close() }

func (f *fakeSAC) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeSAC) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	buf := make([]byte, 4096)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		line := strings.TrimSpace(string(buf[:n]))
		if line == "" {
			_, _ = conn.Write([]byte("\r\nSAC>"))
			continue
		}
		if line == "i" {
			var sb strings.Builder
			sb.WriteString("\r\nSAC is retrieving IP Addresses...\r\n")
			// Sort keys to ensure deterministic NIC enumeration order.
			keys := slices.Sorted(func(yield func(int) bool) {
				for k := range f.ips {
					if !yield(k) {
						return
					}
				}
			})
			for _, num := range keys {
				fmt.Fprintf(&sb, "Net: %d, Ip=%s  Subnet=255.255.255.0  Gateway=10.88.0.1\r\n", num, f.ips[num])
				fmt.Fprintf(&sb, "Net: %d, Ip=fe80::1\r\n", num)
			}
			sb.WriteString("SAC>")
			_, _ = conn.Write([]byte(sb.String()))
			continue
		}
		if strings.HasPrefix(line, "i ") {
			parts := strings.Fields(line)
			if len(parts) == 5 {
				num := 0
				fmt.Sscanf(parts[1], "%d", &num)
				if _, ok := f.ips[num]; ok {
					f.ips[num] = parts[2]
				}
			}
			_, _ = conn.Write([]byte("\r\nSAC>"))
			continue
		}
		_, _ = conn.Write([]byte("\r\nSAC>"))
	}
}

func TestRunQueryNets(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "console.sock")
	fake := newFakeSAC(t, sock, []int{7})
	defer fake.close()

	e := &Executor{WaitReady: 5 * time.Second}
	var out bytes.Buffer
	if err := e.Run(t.Context(), sock, []string{"i"}, nil, &out, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	nums := ParseNetEntries(out.String())
	if len(nums) != 1 || nums[0] != 7 {
		t.Errorf("ParseNetEntries = %v, want [7]", nums)
	}
}

func TestRunSetIPSingleNIC(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "console.sock")
	fake := newFakeSAC(t, sock, []int{7})
	defer fake.close()

	e := &Executor{WaitReady: 5 * time.Second}

	// Set IP.
	if err := e.Run(t.Context(), sock, []string{"i", "7", "10.88.0.60", "255.255.255.0", "10.88.0.1"}, nil, nil, nil); err != nil {
		t.Fatalf("Run set: %v", err)
	}
	if fake.ips[7] != "10.88.0.60" {
		t.Errorf("net 7 ip = %q, want 10.88.0.60", fake.ips[7])
	}

	// Verify via query.
	var out bytes.Buffer
	if err := e.Run(t.Context(), sock, []string{"i"}, nil, &out, nil); err != nil {
		t.Fatalf("Run verify: %v", err)
	}
	if !NetHasIP(out.String(), 7, "10.88.0.60") {
		t.Errorf("NetHasIP = false for net 7 / 10.88.0.60 in %q", out.String())
	}
}

func TestRunMultiNICAllStatic(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "console.sock")
	fake := newFakeSAC(t, sock, []int{2, 4, 6})
	defer fake.close()

	e := &Executor{WaitReady: 5 * time.Second}

	// Query net numbers.
	var out bytes.Buffer
	if err := e.Run(t.Context(), sock, []string{"i"}, nil, &out, nil); err != nil {
		t.Fatalf("Run query: %v", err)
	}
	nums := ParseNetEntries(out.String())
	if len(nums) != 3 {
		t.Fatalf("ParseNetEntries = %v, want 3 entries", nums)
	}

	// Set each NIC.
	ips := []string{"10.0.0.10", "10.0.1.10", "10.0.2.10"}
	for i, ip := range ips {
		if err := e.Run(t.Context(), sock, []string{"i", fmt.Sprintf("%d", nums[i]), ip, "255.255.255.0", "10.0.0.1"}, nil, nil, nil); err != nil {
			t.Fatalf("Run set net %d: %v", nums[i], err)
		}
	}

	// Verify all.
	for num, want := range map[int]string{2: "10.0.0.10", 4: "10.0.1.10", 6: "10.0.2.10"} {
		if fake.ips[num] != want {
			t.Errorf("net %d ip = %q, want %q", num, fake.ips[num], want)
		}
	}
}

func TestRunDialTimeout(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "console.sock")
	// Socket doesn't exist — should timeout.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	e := &Executor{WaitReady: 2 * time.Second}
	err := e.Run(ctx, sock, []string{"i"}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestParseNetEntriesDeduplicatesIPv6(t *testing.T) {
	output := "Net: 3, Ip=169.254.0.1  Subnet=255.255.0.0  Gateway=0.0.0.0\r\n" +
		"Net: 3, Ip=fe80::1\r\n" +
		"Net: 7, Ip=10.88.0.60  Subnet=255.255.255.0  Gateway=10.88.0.1\r\n" +
		"Net: 7, Ip=fe80::2\r\n"
	nums := ParseNetEntries(output)
	if len(nums) != 2 || nums[0] != 3 || nums[1] != 7 {
		t.Errorf("ParseNetEntries = %v, want [3 7]", nums)
	}
}

func TestNetHasIP(t *testing.T) {
	output := "Net: 7, Ip=10.88.0.60  Subnet=255.255.255.0  Gateway=10.88.0.1\r\nSAC>"
	if !NetHasIP(output, 7, "10.88.0.60") {
		t.Error("NetHasIP should return true")
	}
	if NetHasIP(output, 7, "10.88.0.99") {
		t.Error("NetHasIP should return false for wrong IP")
	}
	if NetHasIP(output, 3, "10.88.0.60") {
		t.Error("NetHasIP should return false for wrong net number")
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
