package vm

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// inspectJSON is the on-the-wire shape `cocoon vm inspect --json`
// (and `vm clone --json` / `run --json`) returns. Only the fields
// vk-cocoon needs are decoded; the rest are ignored.
type inspectJSON struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	IP    string `json:"ip"`
	MAC   string `json:"mac"`
	CPU   int    `json:"cpu"`
	Mem   int64  `json:"mem"`
}

// parseInspectJSON unmarshals a single VM record into a *VM.
func parseInspectJSON(raw []byte) (*VM, error) {
	var d inspectJSON
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("decode inspect: %w", err)
	}
	return &VM{
		ID:    d.ID,
		Name:  d.Name,
		State: d.State,
		IP:    d.IP,
		MAC:   d.MAC,
		CPU:   d.CPU,
		Mem:   d.Mem,
	}, nil
}

// vmFromMap is a permissive decoder used by List which gets back a
// JSON array of records — keeping it map-based avoids strict
// structural coupling and lets new fields land server-side without
// breaking the client.
func vmFromMap(m map[string]any) (*VM, error) {
	v := &VM{
		ID:    stringField(m, "id"),
		Name:  stringField(m, "name"),
		State: stringField(m, "state"),
		IP:    stringField(m, "ip"),
		MAC:   stringField(m, "mac"),
	}
	if v.ID == "" {
		return nil, fmt.Errorf("missing id")
	}
	if cpu, ok := numberField(m, "cpu"); ok {
		v.CPU = int(cpu)
	}
	if mem, ok := numberField(m, "mem"); ok {
		v.Mem = int64(mem)
	}
	return v, nil
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func numberField(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case string:
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}
