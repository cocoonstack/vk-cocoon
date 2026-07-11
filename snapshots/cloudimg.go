package snapshots

import (
	"context"
	"io"

	"github.com/cocoonstack/cocoon-common/cloudimg"
	"github.com/cocoonstack/cocoon-common/oci"
)

var _ cloudimg.BlobReader = blobReader{}

// blobReader adapts a Registry to cloudimg.BlobReader.
type blobReader struct {
	registry oci.Registry
	name     string
}

func (b blobReader) ReadBlob(ctx context.Context, digest string) (io.ReadCloser, error) {
	return b.registry.GetBlob(ctx, b.name, digest)
}

func cloudimgStream(ctx context.Context, raw []byte, blobs blobReader, w io.Writer) error {
	return cloudimg.Stream(ctx, raw, blobs, w)
}
