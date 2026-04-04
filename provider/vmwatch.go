package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/projecteru2/core/log"
)

// vmWatchEvent matches the JSON output of `cocoon vm status --event --format json`.
type vmWatchEvent struct {
	Event string         `json:"event"` // ADDED, MODIFIED, DELETED
	VM    vmWatchEventVM `json:"vm"`
}

// vmWatchEventVM is the VM object emitted by cocoon's status event stream.
type vmWatchEventVM struct {
	ID     string `json:"id"`
	Config struct {
		Name    string `json:"name"`
		CPU     int    `json:"cpu"`
		Memory  int64  `json:"memory"`
		Storage int64  `json:"storage"`
		Image   string `json:"image"`
	} `json:"config"`
	State          string `json:"state"`
	PID            int    `json:"pid"`
	NetworkConfigs []struct {
		Mac     string `json:"mac"`
		Network *struct {
			IP string `json:"ip"`
		} `json:"network"`
	} `json:"network_configs"`
	CreatedAt time.Time `json:"created_at"`
}

// vmCache is a thread-safe in-memory cache of VM states populated by the event stream.
type vmCache struct {
	mu  sync.RWMutex
	vms map[string]*cachedVM // keyed by VM ID
}

type cachedVM struct {
	id        string
	name      string
	state     string
	ip        string
	mac       string
	cpu       int
	memoryMB  int
	image     string
	updatedAt time.Time
}

func newVMCache() *vmCache {
	return &vmCache{vms: make(map[string]*cachedVM)}
}

func (c *vmCache) apply(ev vmWatchEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch ev.Event {
	case "ADDED", "MODIFIED":
		c.vms[ev.VM.ID] = eventToCache(ev.VM)
	case "DELETED":
		delete(c.vms, ev.VM.ID)
	}
}

func (c *vmCache) findByID(id string) *cachedVM {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.vms[id]
}

func (c *vmCache) findByName(name string) *cachedVM {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, vm := range c.vms {
		if vm.name == name {
			return vm
		}
	}
	return nil
}

func eventToCache(v vmWatchEventVM) *cachedVM {
	cv := &cachedVM{
		id:        v.ID,
		name:      v.Config.Name,
		state:     v.State,
		cpu:       v.Config.CPU,
		memoryMB:  int(v.Config.Memory / (1024 * 1024)),
		image:     v.Config.Image,
		updatedAt: time.Now(),
	}
	if len(v.NetworkConfigs) > 0 {
		cv.mac = v.NetworkConfigs[0].Mac
		if v.NetworkConfigs[0].Network != nil {
			cv.ip = v.NetworkConfigs[0].Network.IP
		}
	}
	return cv
}

func (cv *cachedVM) toCocoonVM() *CocoonVM {
	return &CocoonVM{
		vmID:     cv.id,
		vmName:   cv.name,
		state:    cv.state,
		ip:       cv.ip,
		mac:      cv.mac,
		cpu:      cv.cpu,
		memoryMB: cv.memoryMB,
		image:    cv.image,
	}
}

// startVMWatcher launches `cocoon vm status --event --format json` and feeds events
// into the cache. It reconnects automatically on process exit.
func (p *CocoonProvider) startVMWatcher(ctx context.Context) {
	p.vmState = newVMCache()
	go p.vmWatchLoop(ctx)
}

// killOrphanEventStreams kills any leftover `cocoon vm status --event` processes
// from previous vk-cocoon instances. These orphans survive vk-cocoon restart
// (KillMode=process) and can interfere with the new event stream's inotify watch.
func killOrphanEventStreams(cocoonBin string) {
	out, err := exec.Command("pgrep", "-f", cocoonBin+" vm status --event").Output() //nolint:gosec
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		_ = exec.Command("kill", line).Run() //nolint:gosec
	}
}

func (p *CocoonProvider) vmWatchLoop(ctx context.Context) {
	logger := log.WithFunc("provider.vmWatchLoop")
	for {
		if err := p.runVMStatusStream(ctx); err != nil {
			logger.Warnf(ctx, "event stream exited: %v, reconnecting in 2s", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (p *CocoonProvider) runVMStatusStream(ctx context.Context) error {
	logger := log.WithFunc("provider.vmWatchStream")
	killOrphanEventStreams(p.cocoonBin)

	args := []string{p.cocoonBin, "vm", "status", "--event", "--format", "json", "--interval", "5"}
	cmd := exec.Command("sudo", args...) //nolint:gosec
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	logger.Info(ctx, "event stream started")

	// Kill the process group when context is canceled or this function returns.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck // best-effort cleanup
		}
	}()
	defer func() {
		close(done)
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck
		}
		_ = cmd.Wait() //nolint:errcheck
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev vmWatchEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			logger.Warnf(ctx, "parse event: %v (line: %.100s)", err, line)
			continue
		}
		p.vmState.apply(ev)

		// Notify pod status for any pod that owns this VM.
		p.notifyPodForVM(ctx, ev.VM.ID, ev.VM.Config.Name)
	}
	return nil
}

// notifyPodForVM finds the pod that owns a VM and triggers a status update.
func (p *CocoonProvider) notifyPodForVM(ctx context.Context, vmID, vmName string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for key, vm := range p.vms {
		if vm.vmID == vmID || vm.vmName == vmName {
			if ns, name, ok := strings.Cut(key, "/"); ok {
				go p.notifyPodStatus(ctx, ns, name)
			}
			return
		}
	}
}
