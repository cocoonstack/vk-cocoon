// Package vm wraps the cocoon CLI as a Runtime interface that the
// rest of vk-cocoon talks to. The default implementation shells out
// to `cocoon ...` subprocesses; tests substitute fakes.
package vm

import (
	"context"
	"io"
)

// VM is the runtime view of a managed cocoon VM.
type VM struct {
	ID    string
	Name  string
	State string
	IP    string
	MAC   string
	CPU   int
	Mem   int64
}

// VM state literals shared by the cocoon CLI and downstream callers.
// Static-mode toolboxes also use StateRunning to fake a healthy
// adopted VM in the in-memory table.
const (
	StateRunning   = "running"
	StateStopped   = "stopped"
	StateImporting = "importing"
)

// CloneOptions is the input to Runtime.Clone.
type CloneOptions struct {
	From     string // source VM name or snapshot ref
	To       string // new VM name
	Network  string
	Storage  string
	NICs     int
	DNS      []string
	NodeName string
}

// RunOptions is the input to Runtime.Run (cold boot from a cloud image).
type RunOptions struct {
	Image    string // cloud image URL or local path
	Name     string
	Network  string
	Storage  string
	NICs     int
	DNS      []string
	OS       string
	NodeName string
}

// ImportOptions is the input to Runtime.SnapshotImport. The Reader
// returned by the Import call streams the snapshot bytes into
// `cocoon snapshot import` via stdin.
type ImportOptions struct {
	Name        string
	Description string
}

// Runtime is the interface vk-cocoon uses to drive cocoon. The
// default implementation lives in cocoon_cli.go and shells out;
// tests inject fakes.
type Runtime interface {
	// Clone clones an existing VM (or restores a snapshot) into a
	// new VM and returns the runtime view.
	Clone(ctx context.Context, opts CloneOptions) (*VM, error)
	// Run boots a fresh VM from a cloud image.
	Run(ctx context.Context, opts RunOptions) (*VM, error)
	// Inspect returns the current state of one VM.
	Inspect(ctx context.Context, vmID string) (*VM, error)
	// List returns every VM the local cocoon runtime knows about.
	List(ctx context.Context) ([]VM, error)
	// Remove destroys a VM.
	Remove(ctx context.Context, vmID string) error
	// SnapshotSave snapshots a running VM in place.
	SnapshotSave(ctx context.Context, vmName, vmID string) error
	// SnapshotImport opens a stdin pipe to `cocoon snapshot import`
	// and returns the writer (caller closes when done) plus a
	// wait function that blocks until the subprocess exits.
	SnapshotImport(ctx context.Context, opts ImportOptions) (io.WriteCloser, func() error, error)
	// SnapshotExport opens a stdout pipe from `cocoon snapshot
	// export` and returns the reader plus a wait function. Used
	// by epoch's snapshot.Pusher to stream a snapshot up.
	SnapshotExport(ctx context.Context, vmName string) (io.ReadCloser, func() error, error)
}
