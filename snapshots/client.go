// Package snapshots wraps the epoch SDK for pulling and pushing cocoon VM snapshots.
package snapshots

import (
	"context"
	"io"

	"github.com/cocoonstack/epoch/registryclient"
)

// RegistryClient is the subset of epoch's HTTP API vk-cocoon needs.
type RegistryClient interface {
	GetManifest(ctx context.Context, name, reference string) ([]byte, string, error)
	PutManifest(ctx context.Context, name, reference string, body []byte, contentType string) error
	DeleteManifest(ctx context.Context, name, reference string) error
	BlobExists(ctx context.Context, name, digest string) (bool, error)
	GetBlob(ctx context.Context, name, digest string) (io.ReadCloser, error)
	PutBlob(ctx context.Context, name, digest string, body io.Reader, size int64) error
}

var _ RegistryClient = (*registryclient.Client)(nil)

// New constructs a RegistryClient for the given epoch base URL and token.
func New(baseURL, token string, opts ...registryclient.Option) RegistryClient {
	return registryclient.New(baseURL, token, opts...)
}
