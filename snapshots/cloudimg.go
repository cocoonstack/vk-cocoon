package snapshots

import (
	"context"
	"io"

	"github.com/cocoonstack/cocoon-common/cloudimg"
	"github.com/cocoonstack/cocoon-common/oci"
)

var _ cloudimg.BlobReader = blobReader{}

type blobReader struct {
	registry oci.Registry
	name     string
}

func (b blobReader) ReadBlob(ctx context.Context, digest string) (io.ReadCloser, error) {
	return b.registry.GetBlob(ctx, b.name, digest)
}
