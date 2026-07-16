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
	NetworkConfigs []struct {
		Tap     string       `json:"tap"`
		Mac     string       `json:"mac"`
		Network *NetworkInfo `json:"network,omitempty"`
	} `json:"network_configs,omitempty"`
}

// snapshotJSON is the wire format of `cocoon snapshot inspect`.
type snapshotJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Image       string `json:"image"`
	ImageDigest string `json:"image_digest"`
	Hypervisor  string `json:"hypervisor"`
}

// parseInspectJSON unmarshals a VM record.
func parseInspectJSON(raw []byte) (*VM, error) {
	var d inspectJSON
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("decode inspect: %w", err)
	}
	return inspectJSONToVM(d), nil
}

// parseVMListJSON unmarshals the VM list output.
// cocoon prints "No VMs found." instead of JSON for an empty list.
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

// parseSnapshotJSON unmarshals a snapshot record.
func parseSnapshotJSON(raw []byte) (*Snapshot, error) {
	var d snapshotJSON
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("decode snapshot inspect: %w", err)
	}
	return &Snapshot{
		ID:          d.ID,
		Name:        d.Name,
		Image:       d.Image,
		ImageDigest: d.ImageDigest,
		Hypervisor:  d.Hypervisor,
	}, nil
}

func inspectJSONToVM(d inspectJSON) *VM {
	v := &VM{
		ID:         d.ID,
		Hypervisor: d.Hypervisor,
		Name:       d.Config.Name,
		State:      d.State,
		PID:        d.PID,
	}
	for _, nc := range d.NetworkConfigs {
		v.NetworkConfigs = append(v.NetworkConfigs, &NetworkConfig{
			Tap:     nc.Tap,
			MAC:     nc.Mac,
			Network: nc.Network,
		})
	}
	if len(d.NetworkConfigs) > 0 {
		v.MAC = d.NetworkConfigs[0].Mac
		if d.NetworkConfigs[0].Network != nil {
			v.IP = d.NetworkConfigs[0].Network.IP
		}
	}
	return v
}
