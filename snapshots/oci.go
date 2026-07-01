package snapshots

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

var (
	_ Registry = (*OCIRegistry)(nil)

	// errBlobUncompressed guards the DiffID/Uncompressed accessors: cocoon blobs
	// are opaque content-addressed bytes and WriteLayer only reads Compressed().
	errBlobUncompressed = errors.New("cocoon blob layers expose only compressed bytes")
)

// OCIRegistry is a Registry backed by a standard OCI Distribution registry
// (e.g. Artifact Registry), using OCI upload sessions and keychain auth.
type OCIRegistry struct {
	base string // registry host + repo prefix, e.g. "asia-docker.pkg.dev/proj/repo"
	opts []remote.Option
}

// NewOCIRegistry roots a client at base, authenticating via keychain (use
// authn.DefaultKeychain for docker config / GCP application-default credentials).
func NewOCIRegistry(base string, keychain authn.Keychain) *OCIRegistry {
	return &OCIRegistry{base: base, opts: []remote.Option{remote.WithAuthFromKeychain(keychain)}}
}

// BaseURL returns the registry root.
func (r *OCIRegistry) BaseURL() string { return r.base }

// GetManifest fetches the raw manifest bytes and media type at repo:tag.
func (r *OCIRegistry) GetManifest(ctx context.Context, repo, tag string) ([]byte, string, error) {
	ref, err := name.ParseReference(r.base + "/" + repo + ":" + tag)
	if err != nil {
		return nil, "", fmt.Errorf("parse ref %s:%s: %w", repo, tag, err)
	}
	desc, err := remote.Get(ref, r.callOpts(ctx)...)
	if err != nil {
		return nil, "", fmt.Errorf("get manifest %s:%s: %w", repo, tag, err)
	}
	return desc.Manifest, string(desc.MediaType), nil
}

// GetBlob streams the blob at the given digest.
func (r *OCIRegistry) GetBlob(ctx context.Context, repo, digest string) (io.ReadCloser, error) {
	ref, err := name.NewDigest(r.base + "/" + repo + "@" + digest)
	if err != nil {
		return nil, fmt.Errorf("parse digest %s@%s: %w", repo, digest, err)
	}
	layer, err := remote.Layer(ref, r.callOpts(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("get blob %s@%s: %w", repo, digest, err)
	}
	return layer.Compressed()
}

// BlobExists reports whether the blob is already present, so pushes can skip it.
func (r *OCIRegistry) BlobExists(ctx context.Context, repo, digest string) (bool, error) {
	ref, err := name.NewDigest(r.base + "/" + repo + "@" + digest)
	if err != nil {
		return false, fmt.Errorf("parse digest %s@%s: %w", repo, digest, err)
	}
	// remote.Layer is lazy; Size() issues the HEAD that reveals whether it exists.
	layer, err := remote.Layer(ref, r.callOpts(ctx)...)
	if err == nil {
		_, err = layer.Size()
	}
	if err == nil {
		return true, nil
	}
	var terr *transport.Error
	if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return false, fmt.Errorf("head blob %s@%s: %w", repo, digest, err)
}

// PutBlob uploads a blob of the given digest/size via a standard upload session.
func (r *OCIRegistry) PutBlob(ctx context.Context, repo, digest string, body io.Reader, size int64) error {
	repoRef, err := name.NewRepository(r.base + "/" + repo)
	if err != nil {
		return fmt.Errorf("parse repo %s: %w", repo, err)
	}
	hash, err := v1.NewHash(digest)
	if err != nil {
		return fmt.Errorf("parse digest %s: %w", digest, err)
	}
	if err := remote.WriteLayer(repoRef, &streamLayer{hash: hash, size: size, body: body}, r.callOpts(ctx)...); err != nil {
		return fmt.Errorf("put blob %s@%s: %w", repo, digest, err)
	}
	return nil
}

// PutManifest uploads a manifest at repo:tag with the given content type.
func (r *OCIRegistry) PutManifest(ctx context.Context, repo, tag string, data []byte, contentType string) error {
	ref, err := name.ParseReference(r.base + "/" + repo + ":" + tag)
	if err != nil {
		return fmt.Errorf("parse ref %s:%s: %w", repo, tag, err)
	}
	if err := remote.Put(ref, rawManifest{data: data, mediaType: types.MediaType(contentType)}, r.callOpts(ctx)...); err != nil {
		return fmt.Errorf("put manifest %s:%s: %w", repo, tag, err)
	}
	return nil
}

// DeleteManifest removes the manifest at repo:reference (tag or digest).
func (r *OCIRegistry) DeleteManifest(ctx context.Context, repo, reference string) error {
	// A digest (sha256:...) joins the repo with '@'; a tag with ':'.
	sep := ":"
	if strings.ContainsRune(reference, ':') {
		sep = "@"
	}
	ref, err := name.ParseReference(r.base + "/" + repo + sep + reference)
	if err != nil {
		return fmt.Errorf("parse ref %s%s%s: %w", repo, sep, reference, err)
	}
	if err := remote.Delete(ref, r.callOpts(ctx)...); err != nil {
		return fmt.Errorf("delete manifest %s: %w", reference, err)
	}
	return nil
}

func (r *OCIRegistry) callOpts(ctx context.Context) []remote.Option {
	return append(r.opts, remote.WithContext(ctx))
}

// streamLayer is a v1.Layer over a body with a known digest and size, so PutBlob
// streams a raw blob without buffering it (WriteLayer reads only Compressed()).
// body is single-use: a retried upload fails the digest check, not corrupts.
type streamLayer struct {
	hash v1.Hash
	size int64
	body io.Reader
}

func (l *streamLayer) Digest() (v1.Hash, error)             { return l.hash, nil }
func (l *streamLayer) Size() (int64, error)                 { return l.size, nil }
func (l *streamLayer) Compressed() (io.ReadCloser, error)   { return io.NopCloser(l.body), nil }
func (l *streamLayer) MediaType() (types.MediaType, error)  { return types.OCILayer, nil }
func (l *streamLayer) DiffID() (v1.Hash, error)             { return v1.Hash{}, errBlobUncompressed }
func (l *streamLayer) Uncompressed() (io.ReadCloser, error) { return nil, errBlobUncompressed }

// rawManifest is a remote.Taggable over pre-serialized manifest bytes.
type rawManifest struct {
	data      []byte
	mediaType types.MediaType
}

func (m rawManifest) RawManifest() ([]byte, error)        { return m.data, nil }
func (m rawManifest) MediaType() (types.MediaType, error) { return m.mediaType, nil }
