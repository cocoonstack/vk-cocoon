package snapshots

import (
	"context"
	"io"

	"github.com/cocoonstack/epoch/snapshot"
	"github.com/cocoonstack/vk-cocoon/vm"
)

var _ snapshot.CocoonRunner = runnerAdapter{}

// runnerAdapter wraps vm.Runtime to satisfy epoch's snapshot.CocoonRunner.
type runnerAdapter struct {
	Runtime vm.Runtime
}

// Export forwards to vm.Runtime.SnapshotExport.
func (a runnerAdapter) Export(ctx context.Context, name string) (io.ReadCloser, func() error, error) {
	return a.Runtime.SnapshotExport(ctx, name)
}

// Import forwards to vm.Runtime.SnapshotImport.
func (a runnerAdapter) Import(ctx context.Context, opts snapshot.ImportOptions) (io.WriteCloser, func() error, error) {
	return a.Runtime.SnapshotImport(ctx, vm.ImportOptions{
		Name:        opts.Name,
		Description: opts.Description,
	})
}
