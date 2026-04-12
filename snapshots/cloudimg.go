package snapshots

import (
	"context"
	"io"

	"github.com/cocoonstack/epoch/cloudimg"
)

// blobReader adapts RegistryClient to cloudimg.BlobReader.
type blobReader struct {
	client RegistryClient
	name   string
}

// ReadBlob fetches a blob by digest.
func (b blobReader) ReadBlob(ctx context.Context, digest string) (io.ReadCloser, error) {
	return b.client.GetBlob(ctx, b.name, digest)
}

// cloudimgStream wraps cloudimg.Stream.
func cloudimgStream(ctx context.Context, raw []byte, blobs blobReader, w io.Writer) error {
	return cloudimg.Stream(ctx, raw, blobs, w)
}
