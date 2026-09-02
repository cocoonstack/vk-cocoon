package snapshots

import (
	"context"
	"io"

	"github.com/cocoonstack/cocoon-common/snapshot"
	"github.com/cocoonstack/vk-cocoon/vm"
)

var _ snapshot.CocoonRunner = runnerAdapter{}

type runnerAdapter struct {
	Runtime vm.Runtime
}

func (a runnerAdapter) Export(ctx context.Context, name string) (io.ReadCloser, func() error, error) {
	return a.Runtime.SnapshotExport(ctx, name)
}
