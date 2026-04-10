package snapshots

import (
	"context"
	"io"

	"github.com/cocoonstack/epoch/snapshot"

	"github.com/cocoonstack/vk-cocoon/vm"
)

// runnerAdapter wraps a vm.Runtime so it satisfies epoch's
// snapshot.CocoonRunner interface (Export + Import). The two
// interfaces have the same shape but live in different packages;
// this glue keeps the dependency direction one-way (vk-cocoon ->
// epoch, not the other way around).
type runnerAdapter struct {
	Runtime vm.Runtime
}

// Compile-time check that runnerAdapter satisfies the epoch
// snapshot.CocoonRunner contract.
var _ snapshot.CocoonRunner = runnerAdapter{}

// Export forwards to the underlying vm.Runtime.SnapshotExport.
func (a runnerAdapter) Export(ctx context.Context, name string) (io.ReadCloser, func() error, error) {
	return a.Runtime.SnapshotExport(ctx, name)
}

// Import forwards to the underlying vm.Runtime.SnapshotImport.
func (a runnerAdapter) Import(ctx context.Context, opts snapshot.ImportOptions) (io.WriteCloser, func() error, error) {
	return a.Runtime.SnapshotImport(ctx, vm.ImportOptions{
		Name:        opts.Name,
		Description: opts.Description,
	})
}
