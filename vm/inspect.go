package vm

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// inspectJSON is the on-the-wire shape `cocoon vm inspect --json`
// (and `vm clone --json` / `run --json`) returns. Only the fields
// vk-cocoon needs are decoded; the rest are ignored.
type inspectJSON struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Config struct {
		Name   string `json:"name"`
		CPU    int    `json:"cpu"`
		Memory int64  `json:"memory"`
	} `json:"config"`
	NetworkConfigs []struct {
		Mac     string `json:"mac"`
		Network *struct {
			IP string `json:"ip"`
		} `json:"network,omitempty"`
	} `json:"network_configs,omitempty"`
}

// snapshotJSON is the subset of `cocoon snapshot inspect` output
// vk-cocoon needs to restore a clone on another node.
type snapshotJSON struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Image string `json:"image"`
}

// parseInspectJSON unmarshals a single VM record into a *VM.
func parseInspectJSON(raw []byte) (*VM, error) {
	var d inspectJSON
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("decode inspect: %w", err)
	}
	return inspectJSONToVM(d), nil
}

// parseVMListJSON unmarshals the `cocoon vm list -o json` output into VMs.
// cocoon prints a plain "No VMs found." line instead of JSON for an empty list.
func parseVMListJSON(raw []byte) ([]VM, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("No VMs found.")) {
		return nil, nil
	}

	var docs []inspectJSON
	if err := json.Unmarshal(trimmed, &docs); err != nil {
		return nil, fmt.Errorf("decode vm list: %w", err)
	}

	out := make([]VM, 0, len(docs))
	for _, doc := range docs {
		if doc.ID == "" {
			continue
		}
		out = append(out, *inspectJSONToVM(doc))
	}
	return out, nil
}

// parseSnapshotJSON unmarshals a single snapshot record into a *Snapshot.
func parseSnapshotJSON(raw []byte) (*Snapshot, error) {
	var d snapshotJSON
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("decode snapshot inspect: %w", err)
	}
	return &Snapshot{
		ID:    d.ID,
		Name:  d.Name,
		Image: d.Image,
	}, nil
}

func inspectJSONToVM(d inspectJSON) *VM {
	v := &VM{
		ID:    d.ID,
		Name:  d.Config.Name,
		State: d.State,
		CPU:   d.Config.CPU,
		Mem:   d.Config.Memory,
	}
	if len(d.NetworkConfigs) == 0 {
		return v
	}

	v.MAC = d.NetworkConfigs[0].Mac
	if d.NetworkConfigs[0].Network != nil {
		v.IP = d.NetworkConfigs[0].Network.IP
	}
	return v
}
