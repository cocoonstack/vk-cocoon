package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/projecteru2/core/log"
)

type vmWatchEvent struct {
	Event string         `json:"event"`
	VM    vmWatchEventVM `json:"vm"`
}

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

type vmCache struct {
	mu    sync.RWMutex
	vms   map[string]*cachedVM
	names map[string]string // name → id index

	waiterMu sync.Mutex
	waiters  map[string][]chan struct{} // vmID → channels
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
	return &vmCache{
		vms:     make(map[string]*cachedVM),
		names:   make(map[string]string),
		waiters: make(map[string][]chan struct{}),
	}
}

func (c *vmCache) apply(ev vmWatchEvent) {
	c.mu.Lock()
	switch ev.Event {
	case "ADDED", "MODIFIED":
		old := c.vms[ev.VM.ID]
		if old != nil && old.name != "" && old.name != ev.VM.Config.Name {
			delete(c.names, old.name)
		}
		cv := eventToCache(ev.VM)
		c.vms[ev.VM.ID] = cv
		if cv.name != "" {
			c.names[cv.name] = cv.id
		}
	case "DELETED":
		if old := c.vms[ev.VM.ID]; old != nil && old.name != "" {
			delete(c.names, old.name)
		}
		delete(c.vms, ev.VM.ID)
	}
	c.mu.Unlock()

	if ev.Event == "ADDED" || ev.Event == "MODIFIED" {
		c.notifyWaiters(ev.VM.ID)
		if ev.VM.Config.Name != "" {
			c.notifyWaiters(ev.VM.Config.Name)
		}
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
	if id, ok := c.names[name]; ok {
		return c.vms[id]
	}
	return nil
}

func (c *vmCache) evictByID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.vms[id]; old != nil && old.name != "" {
		delete(c.names, old.name)
	}
	delete(c.vms, id)
}

func (c *vmCache) waitFor(key string) chan struct{} {
	ch := make(chan struct{}, 1)
	c.waiterMu.Lock()
	c.waiters[key] = append(c.waiters[key], ch)
	c.waiterMu.Unlock()
	return ch
}

func (c *vmCache) notifyWaiters(key string) {
	c.waiterMu.Lock()
	chs := c.waiters[key]
	delete(c.waiters, key)
	c.waiterMu.Unlock()
	for _, ch := range chs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
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

func (p *CocoonProvider) startVMWatcher(ctx context.Context) {
	p.vmState = newVMCache()
	go p.vmWatchLoop(ctx)
}

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
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start event stream: %w", err)
	}
	logger.Info(ctx, "event stream started")

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck
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
		p.notifyPodForVM(ctx, ev.VM.ID, ev.VM.Config.Name)
	}
	return nil
}

func (p *CocoonProvider) notifyPodForVM(ctx context.Context, vmID, vmName string) {
	p.mu.RLock()
	key := p.vmIDToPod[vmID]
	if key == "" {
		key = p.vmNameToPod[vmName]
	}
	p.mu.RUnlock()
	if key == "" {
		return
	}
	if ns, name, ok := strings.Cut(key, "/"); ok {
		go p.notifyPodStatus(ctx, ns, name)
	}
}

func (p *CocoonProvider) awaitVMInCache(ctx context.Context, vmID, vmName string, timeout time.Duration) *CocoonVM {
	if p.vmState == nil {
		return nil
	}
	if cv := p.vmState.findByID(vmID); cv != nil {
		return cv.toCocoonVM()
	}
	if vmName != "" {
		if cv := p.vmState.findByName(vmName); cv != nil {
			return cv.toCocoonVM()
		}
	}

	key := vmID
	if key == "" {
		key = vmName
	}
	ch := p.vmState.waitFor(key)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	case <-ctx.Done():
		return nil
	}

	if vmID != "" {
		if cv := p.vmState.findByID(vmID); cv != nil {
			return cv.toCocoonVM()
		}
	}
	if vmName != "" {
		if cv := p.vmState.findByName(vmName); cv != nil {
			return cv.toCocoonVM()
		}
	}
	return nil
}
