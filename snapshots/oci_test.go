package snapshots

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/registry"
)

// TestOCIRegistryRoundTrip exercises the full Registry surface against an
// in-memory OCI registry: a blob and a custom-artifactType manifest survive a
// put -> exists -> get -> delete round trip.
func TestOCIRegistryRoundTrip(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)

	r := NewOCIRegistry(strings.TrimPrefix(srv.URL, "http://")+"/cocoon", authn.DefaultKeychain)
	ctx := context.Background()

	blob := []byte("hello cocoon blob")
	sum := sha256.Sum256(blob)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	if ok, err := r.BlobExists(ctx, "myvm", digest); err != nil || ok {
		t.Fatalf("BlobExists before put = (%v, %v), want (false, nil)", ok, err)
	}
	if err := r.PutBlob(ctx, "myvm", digest, bytes.NewReader(blob), int64(len(blob))); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if ok, err := r.BlobExists(ctx, "myvm", digest); err != nil || !ok {
		t.Fatalf("BlobExists after put = (%v, %v), want (true, nil)", ok, err)
	}

	rc, err := r.GetBlob(ctx, "myvm", digest)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, blob) {
		t.Fatalf("GetBlob = %q, want %q", got, blob)
	}

	const mt = "application/vnd.oci.image.manifest.v1+json"
	mf := []byte(`{"schemaVersion":2,"mediaType":"` + mt +
		`","artifactType":"application/vnd.cocoonstack.snapshot.v1+json","config":{"mediaType":` +
		`"application/vnd.cocoonstack.snapshot.config.v1+json","digest":"` + digest +
		`","size":` + strconv.Itoa(len(blob)) + `},"layers":[]}`)
	if err := r.PutManifest(ctx, "myvm", "hibernate", mf, mt); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	raw, gotMT, err := r.GetManifest(ctx, "myvm", "hibernate")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if !bytes.Equal(raw, mf) {
		t.Fatalf("GetManifest bytes mismatch")
	}
	if gotMT != mt {
		t.Fatalf("GetManifest mediaType = %q, want %q", gotMT, mt)
	}

	if err := r.DeleteManifest(ctx, "myvm", "hibernate"); err != nil {
		t.Fatalf("DeleteManifest: %v", err)
	}
	if _, _, err := r.GetManifest(ctx, "myvm", "hibernate"); err == nil {
		t.Fatal("GetManifest after delete: want error, got nil")
	}
}
