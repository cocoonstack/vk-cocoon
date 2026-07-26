package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// StateRunning is the state string cocoon reports for a live VM.
	StateRunning = "running"

	RestoreCopy     RestoreMode = "copy"
	RestoreOnDemand RestoreMode = "ondemand"
	RestoreMmap     RestoreMode = "mmap"
)

var (
	// ErrVMNotFound signals the cocoon CLI has authoritatively reported the
	// VM does not exist. Callers use this to distinguish a gone VM from a
	// transient CLI failure (sudo hiccup, timeout, parse error) where the
	// VM may still be alive. Returned wrapped; unwrap with errors.Is.
	ErrVMNotFound = errors.New("vm not found")

	// ErrImageNotFound signals the cocoon CLI has authoritatively reported
	// the image is not stored locally. Used by Puller.EnsureCloudImageFromRaw
	// as the idempotency probe for `cocoon image inspect`.
	ErrImageNotFound = errors.New("image not found")

	// ErrSnapshotNotFound is the sibling of ErrVMNotFound for snapshot inspect.
	ErrSnapshotNotFound = errors.New("snapshot not found")
)

// NetworkInfo holds CNI-assigned addressing for a NIC. Nil for DHCP networks.
type NetworkInfo struct {
	IP      string `json:"ip"`
	Gateway string `json:"gateway"`
	Prefix  int    `json:"prefix"`
}

// NetworkConfig is a single NIC configuration from cocoon vm inspect.
type NetworkConfig struct {
	Tap     string       `json:"tap"`
	MAC     string       `json:"mac"`
	Network *NetworkInfo `json:"network,omitempty"`
}

// VM is the runtime view of a cocoon VM.
type VM struct {
	ID             string
	Name           string
	Hypervisor     string
	State          string
	IP             string
	MAC            string
	PID            int
	NetworkConfigs []*NetworkConfig
}

// Snapshot is the subset of `cocoon snapshot inspect` needed to restore
// a VM. Image is required for cloudimg-backed snapshots' qcow2 backing chain.
// Hypervisor records which backend ("cloud-hypervisor" or "firecracker")
// produced the snapshot, so vk-cocoon can reject backend-mismatched clones
// before shelling out to cocoon.
type Snapshot struct {
	ID          string
	Name        string
	Image       string
	ImageDigest string // resolved image digest (e.g. "sha256:abc...")
	Hypervisor  string
}

// RestoreMode maps to `cocoon vm clone --restore-mode`: how CH restores guest
// memory. RestoreMmap needs a CH build with mmap restore support.
type RestoreMode string

// ParseRestoreMode validates a configured restore mode, normalizing case.
func ParseRestoreMode(s string) (RestoreMode, error) {
	switch m := RestoreMode(strings.ToLower(strings.TrimSpace(s))); m {
	case RestoreCopy, RestoreOnDemand, RestoreMmap:
		return m, nil
	default:
		return "", fmt.Errorf("restore mode must be %s, %s or %s, got %q",
			RestoreCopy, RestoreOnDemand, RestoreMmap, s)
	}
}

// CloneOptions is the input to Runtime.Clone. Resource fields inherit
// from the snapshot unless overridden.
type CloneOptions struct {
	From        string
	To          string
	Network     string
	Backend     string
	NoDirectIO  bool
	Pull        bool
	RestoreMode RestoreMode
	// FromDir maps to `cocoon vm clone --from-dir`: when set, From is
	// ignored and --pull is forced (the dir holds snapshot data, not
	// base image layers).
	FromDir string
	// NICs overrides the snapshot's NIC count when non-nil.
	NICs *int
}

// RunOptions is the input to Runtime.Run.
type RunOptions struct {
	Image      string
	Name       string
	CPU        int
	Memory     string
	Network    string
	Storage    string
	OS         string
	Backend    string
	NoDirectIO bool
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
	Exec(ctx context.Context, vmID string, argv []string, env map[string]string, stdin io.Reader, stdout, stderr io.Writer) error
	Logs(ctx context.Context, vmID string, tail int) (io.ReadCloser, error)
	SnapshotSave(ctx context.Context, vmName, vmID string) error
	SnapshotRemoveIfExists(ctx context.Context, name string) error
	Snapshot(ctx context.Context, name string) (*Snapshot, error)
	SnapshotImport(ctx context.Context, name string) (io.WriteCloser, func() error, error)
	SnapshotExport(ctx context.Context, vmName string) (io.ReadCloser, func() error, error)
	EnsureImage(ctx context.Context, image string, force bool) error
	// Image probes local presence via `cocoon image inspect`;
	// absence is ErrImageNotFound.
	Image(ctx context.Context, name string) error
	ImageImport(ctx context.Context, name string) (io.WriteCloser, func() error, error)
	WatchEvents(ctx context.Context) (<-chan VMEvent, error)
	// NetResize hot-resizes a live VM's NIC count.
	NetResize(ctx context.Context, vmID string, target int) error
}
