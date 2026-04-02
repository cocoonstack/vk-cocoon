package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

const podMapDir = "/var/lib/cocoon/vk-cocoon"

// podMap persists podKey → VM metadata to disk so vk-cocoon can recover
// pod-to-VM mappings after a restart without relying on name heuristics.
type podMap struct {
	mu      sync.RWMutex
	path    string
	entries map[string]podMapEntry
}

type podMapEntry struct {
	VMID    string `json:"vm_id"`
	VMName  string `json:"vm_name"`
	Image   string `json:"image"`
	Network string `json:"network,omitempty"`
}

func newPodMap() *podMap {
	pm := &podMap{
		path:    filepath.Join(podMapDir, "pods.json"),
		entries: make(map[string]podMapEntry),
	}
	pm.load()
	return pm
}

func (pm *podMap) load() {
	data, err := os.ReadFile(pm.path) //nolint:gosec
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &pm.entries)
}

func (pm *podMap) save() {
	_ = os.MkdirAll(filepath.Dir(pm.path), 0o750)
	data, err := json.MarshalIndent(pm.entries, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(pm.path, data, 0o640) //nolint:gosec
}

// Store records a pod → VM mapping and persists to disk.
func (pm *podMap) Store(key string, vmID, vmName, image string) {
	if pm == nil {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.entries[key] = podMapEntry{VMID: vmID, VMName: vmName, Image: image}
	pm.save()
}

// Delete removes a pod mapping and persists to disk.
func (pm *podMap) Delete(key string) {
	if pm == nil {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.entries, key)
	pm.save()
}

// Lookup returns the persisted VM metadata for a pod key.
func (pm *podMap) Lookup(key string) (podMapEntry, bool) {
	if pm == nil {
		return podMapEntry{}, false
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	e, ok := pm.entries[key]
	return e, ok
}
