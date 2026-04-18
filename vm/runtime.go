// Package vm wraps the cocoon CLI as a Runtime interface. Tests substitute fakes.
package vm

import (
	"context"
	"io"
)

// StateRunning is the state string cocoon reports for a live VM.
const StateRunning = "running"

// NetworkInfo holds CNI-assigned addressing for a NIC. Nil for DHCP networks.
type NetworkInfo struct {
	IP      string `json:"ip"`
	Gateway string `json:"gateway"`
	Prefix  int    `json:"prefix"`
}

// NetworkConfig is a single NIC configuration from cocoon vm inspect.
type NetworkConfig struct {
	Tap       string       `json:"tap"`
	MAC       string       `json:"mac"`
	NetNSPath string       `json:"netns_path"`
	Network   *NetworkInfo `json:"network,omitempty"`
}

// VM is the runtime view of a cocoon VM.
type VM struct {
	ID             string
	Name           string
	Hypervisor     string
	State          string
	IP             string
	MAC            string
	CPU            int
	Mem            int64
	PID            int
	NetworkConfigs []*NetworkConfig
}

// Snapshot is the subset of `cocoon snapshot inspect` needed to restore
// a VM. Image is required for cloudimg-backed snapshots' qcow2 backing chain.
// Hypervisor records which backend ("cloud-hypervisor" or "firecracker")
// produced the snapshot, so vk-cocoon can reject backend-mismatched clones
// before shelling out to cocoon.
type Snapshot struct {
	ID         string
	Name       string
	Image      string
	Hypervisor string
}

// CloneOptions is the input to Runtime.Clone.
type CloneOptions struct {
	From       string
	To         string
	CPU        int
	Memory     string
	Network    string
	Storage    string
	NICs       int
	DNS        []string
	Backend    string
	NoDirectIO bool
}

// RunOptions is the input to Runtime.Run.
type RunOptions struct {
	Image      string
	Name       string
	CPU        int
	Memory     string
	Network    string
	Storage    string
	NICs       int
	DNS        []string
	OS         string
	Force      bool
	Backend    string
	NoDirectIO bool
}

// ImportOptions is the input to Runtime.SnapshotImport.
type ImportOptions struct {
	Name        string
	Description string
}

// VMEvent is a single event from the cocoon event stream.
type VMEvent struct {
	Event string `json:"event"` // ADDED, MODIFIED, DELETED
	VM    VM     `json:"vm"`
}

// Runtime is the interface vk-cocoon uses to drive cocoon.
type Runtime interface {
	Clone(ctx context.Context, opts CloneOptions) (*VM, error)
	Run(ctx context.Context, opts RunOptions) (*VM, error)
	Inspect(ctx context.Context, vmID string) (*VM, error)
	List(ctx context.Context) ([]VM, error)
	Remove(ctx context.Context, vmID string) error
	Start(ctx context.Context, vmID string) error
	SnapshotSave(ctx context.Context, vmName, vmID string) error
	Snapshot(ctx context.Context, name string) (*Snapshot, error)
	SnapshotImport(ctx context.Context, opts ImportOptions) (io.WriteCloser, func() error, error)
	SnapshotExport(ctx context.Context, vmName string) (io.ReadCloser, func() error, error)
	EnsureImage(ctx context.Context, image string, force bool) error
	WatchEvents(ctx context.Context) (<-chan VMEvent, error)
}
