package snapshots

import (
	"context"
	"io"

	"github.com/cocoonstack/cocoon-common/snapshot"
	"github.com/cocoonstack/vk-cocoon/vm"
)

var _ snapshot.CocoonRunner = runnerAdapter{}

// runnerAdapter wraps vm.Runtime to satisfy snapshot.CocoonRunner.
type runnerAdapter struct {
	Runtime vm.Runtime
}

func (a runnerAdapter) Export(ctx context.Context, name string) (io.ReadCloser, func() error, error) {
	return a.Runtime.SnapshotExport(ctx, name)
}

func (a runnerAdapter) Import(ctx context.Context, opts snapshot.ImportOptions) (io.WriteCloser, func() error, error) {
	return a.Runtime.SnapshotImport(ctx, vm.ImportOptions{
		Name:        opts.Name,
		Description: opts.Description,
	})
}
