package snapshots

import (
	"context"
	"io"

	"github.com/cocoonstack/epoch/cloudimg"
)

// blobReader adapts a RegistryClient to the cloudimg.BlobReader
// interface so cloudimg.Stream can fetch each layer through the
// same client the rest of the package uses.
type blobReader struct {
	client RegistryClient
	name   string
}

// ReadBlob satisfies cloudimg.BlobReader.
func (b blobReader) ReadBlob(ctx context.Context, digest string) (io.ReadCloser, error) {
	return b.client.GetBlob(ctx, b.name, digest)
}

// cloudimgStream is a thin wrapper that hides the cloudimg import
// from puller.go so the puller stays focused on orchestration.
func cloudimgStream(ctx context.Context, raw []byte, blobs blobReader, w io.Writer) error {
	return cloudimg.Stream(ctx, raw, blobs, w)
}
