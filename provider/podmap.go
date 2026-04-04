package provider

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"sync"
)

const podMapDir = "/var/lib/cocoon/vk-cocoon"

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
	pm.mu.RLock()
	snapshot := make(map[string]podMapEntry, len(pm.entries))
	maps.Copy(snapshot, pm.entries)
	pm.mu.RUnlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(pm.path), 0o750)
	_ = os.WriteFile(pm.path, data, 0o640) //nolint:gosec
}

func (pm *podMap) Store(key string, vmID, vmName, image string) {
	if pm == nil {
		return
	}
	pm.mu.Lock()
	pm.entries[key] = podMapEntry{VMID: vmID, VMName: vmName, Image: image}
	pm.mu.Unlock()
	pm.save()
}

func (pm *podMap) Delete(key string) {
	if pm == nil {
		return
	}
	pm.mu.Lock()
	delete(pm.entries, key)
	pm.mu.Unlock()
	pm.save()
}

func (pm *podMap) Lookup(key string) (podMapEntry, bool) {
	if pm == nil {
		return podMapEntry{}, false
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	e, ok := pm.entries[key]
	return e, ok
}
