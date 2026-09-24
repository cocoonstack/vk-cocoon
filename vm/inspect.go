package vm

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// inspectJSON is the wire format of `cocoon vm inspect --json`.
type inspectJSON struct {
	ID         string `json:"id"`
	Hypervisor string `json:"hypervisor"`
	State      string `json:"state"`
	PID        int    `json:"pid"`
	Config     struct {
		Name string `json:"name"`
	} `json:"config"`
	NetworkConfigs []*NetworkConfig `json:"network_configs,omitempty"`
}

func parseInspectJSON(raw []byte) (*VM, error) {
	var d inspectJSON
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("decode inspect: %w", err)
	}
	return inspectJSONToVM(d), nil
}

// parseVMListJSON handles cocoon printing "No VMs found." instead of JSON for an empty list.
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

func parseSnapshotJSON(raw []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("decode snapshot inspect: %w", err)
	}
	return &s, nil
}

func inspectJSONToVM(d inspectJSON) *VM {
	v := &VM{
		ID:             d.ID,
		Hypervisor:     d.Hypervisor,
		Name:           d.Config.Name,
		State:          d.State,
		PID:            d.PID,
		NetworkConfigs: d.NetworkConfigs,
	}
	if len(d.NetworkConfigs) > 0 {
		v.MAC = d.NetworkConfigs[0].MAC
		if d.NetworkConfigs[0].Network != nil {
			v.IP = d.NetworkConfigs[0].Network.IP
		}
	}
	return v
}
