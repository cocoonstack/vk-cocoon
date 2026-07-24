package cocoon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon-common/manifest"
	"github.com/cocoonstack/cocoon-common/ociutil"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestResolveWakeSourceVerifiedLocalHit(t *testing.T) {
	rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{
		"vk-ns-demo-0": {Name: "vk-ns-demo-0", ID: "SNAP-1"},
	}}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Registry = newWakeVerifyRegistry(t, "SNAP-1")

	source, snapshot, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0")
	if err != nil {
		t.Fatalf("resolveWakeSource: %v", err)
	}
	if source != "vk-ns-demo-0" || snapshot == nil || snapshot.ID != "SNAP-1" {
		t.Errorf("source = %q snapshot = %+v, want verified local snapshot", source, snapshot)
	}
	if len(rt.snapshotRemoveCalls) != 0 {
		t.Errorf("verified hit must not remove snapshots, got %v", rt.snapshotRemoveCalls)
	}
}

func TestResolveWakeSourceStaleLocalDiscardsAndPulls(t *testing.T) {
	rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{
		"vk-ns-demo-0": {Name: "vk-ns-demo-0", ID: "SNAP-OLD"},
	}}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Registry = newWakeVerifyRegistry(t, "SNAP-NEW")

	_, _, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0")
	// Puller is nil, so reaching the pull path surfaces as this exact error —
	// proof the stale local was rejected rather than used.
	if err == nil || !strings.Contains(err.Error(), "no puller configured") {
		t.Fatalf("err = %v, want fall-through to pull path", err)
	}
	if !slices.Contains(rt.snapshotRemoveCalls, "vk-ns-demo-0") ||
		!slices.Contains(rt.snapshotRemoveCalls, forkSnapshotName("vk-ns-demo-0")) {
		t.Errorf("stale local must be removed, got %v", rt.snapshotRemoveCalls)
	}
}

func TestResolveWakeSourceNoTagDiscardsLocal(t *testing.T) {
	rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{
		"vk-ns-demo-0": {Name: "vk-ns-demo-0", ID: "SNAP-1"},
	}}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Registry = wakeVerifyRegistry{tagExists: false}

	_, _, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0")
	if err == nil || !strings.Contains(err.Error(), "no puller configured") {
		t.Fatalf("err = %v, want fall-through to pull path", err)
	}
	if len(rt.snapshotRemoveCalls) == 0 {
		t.Error("local snapshot without a registry tag must be discarded")
	}
}

func TestResolveWakeSourceRegistryErrorFailsClosed(t *testing.T) {
	rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{
		"vk-ns-demo-0": {Name: "vk-ns-demo-0", ID: "SNAP-1"},
	}}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Registry = wakeVerifyRegistry{tagExists: false, hasManifestErr: errors.New("registry down")}

	_, _, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0")
	if err == nil || !strings.Contains(err.Error(), "registry down") {
		t.Fatalf("err = %v, want fail-closed verification error", err)
	}
	if len(rt.snapshotRemoveCalls) != 0 {
		t.Errorf("unverifiable local must be kept, got removes %v", rt.snapshotRemoveCalls)
	}
}

// wakeVerifyRegistry serves one hibernate-tag artifact (manifest + config
// blob) so resolveWakeSource's local-cache verification can run against it.
type wakeVerifyRegistry struct {
	fakeRegistry
	tagExists      bool
	hasManifestErr error
	manifestRaw    []byte
	blobs          map[string][]byte
}

func newWakeVerifyRegistry(t *testing.T, snapshotID string) wakeVerifyRegistry {
	t.Helper()
	raw, blobs := hibernateArtifact(t, snapshotID)
	return wakeVerifyRegistry{tagExists: true, manifestRaw: raw, blobs: blobs}
}

func (r wakeVerifyRegistry) HasManifest(context.Context, string, string) (bool, error) {
	return r.tagExists, r.hasManifestErr
}

func (r wakeVerifyRegistry) GetManifest(context.Context, string, string) ([]byte, string, error) {
	return r.manifestRaw, "", nil
}

func (r wakeVerifyRegistry) GetBlob(_ context.Context, _, digest string) (io.ReadCloser, error) {
	b, ok := r.blobs[digest]
	if !ok {
		return nil, errors.New("blob not found")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// hibernateArtifact builds the registry view of a pushed snapshot whose
// export came from the local snapshot with the given ID.
func hibernateArtifact(t *testing.T, snapshotID string) ([]byte, map[string][]byte) {
	t.Helper()
	cfg := manifest.SnapshotConfig{SchemaVersion: "v1", SnapshotID: snapshotID}
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + ociutil.SHA256Hex(cfgBytes)
	m := manifest.OCIManifest{
		SchemaVersion: 2,
		MediaType:     manifest.MediaTypeOCIManifest,
		ArtifactType:  manifest.ArtifactTypeSnapshot,
		Config: manifest.Descriptor{
			MediaType: manifest.MediaTypeSnapshotConfig,
			Digest:    digest,
			Size:      int64(len(cfgBytes)),
		},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw, map[string][]byte{digest: cfgBytes}
}
