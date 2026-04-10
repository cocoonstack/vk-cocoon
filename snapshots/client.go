// Package snapshots wraps the epoch SDK so vk-cocoon can pull and
// push cocoon VM snapshots and cloud images without speaking OCI
// distribution directly. The interfaces here are the seam tests use
// to substitute fakes; the production glue is in puller.go and
// pusher.go.
package snapshots

import (
	"context"
	"io"

	"github.com/cocoonstack/epoch/registryclient"
)

// RegistryClient is the subset of epoch's HTTP API vk-cocoon needs.
// It is satisfied by *registryclient.Client at compile time so the
// production wiring is "free", and tests can drop in a fake without
// touching epoch.
type RegistryClient interface {
	GetManifest(ctx context.Context, name, reference string) ([]byte, string, error)
	PutManifest(ctx context.Context, name, reference string, body []byte, contentType string) error
	DeleteManifest(ctx context.Context, name, reference string) error
	BlobExists(ctx context.Context, name, digest string) (bool, error)
	GetBlob(ctx context.Context, name, digest string) (io.ReadCloser, error)
	PutBlob(ctx context.Context, name, digest string, body io.Reader, size int64) error
}

// Compile-time guarantee.
var _ RegistryClient = (*registryclient.Client)(nil)

// New constructs the production RegistryClient against the supplied
// base URL and (optional) bearer token.
func New(baseURL, token string) RegistryClient {
	return registryclient.New(baseURL, token)
}
