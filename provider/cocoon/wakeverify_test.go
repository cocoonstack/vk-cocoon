package cocoon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cocoonstack/cocoon-common/manifest"
	"github.com/cocoonstack/cocoon-common/meta"
	"github.com/cocoonstack/cocoon-common/ociutil"
	"github.com/cocoonstack/cocoon-common/snapshot"

	"github.com/cocoonstack/vk-cocoon/snapshots"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestResolveWakeSourceVerifiedLocalHit(t *testing.T) {
	rt := &fakeRuntime{snapshots: map[string]*vm.Snapshot{
		"vk-ns-demo-0": {Name: "vk-ns-demo-0", ID: "SNAP-1"},
	}}
	p := newTestProvider(t)
	p.Runtime = rt
	p.Registry = newWakeVerifyRegistry(t, "SNAP-1")

	src, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0")
	if err != nil {
		t.Fatalf("resolveWakeSource: %v", err)
	}
	if src.localName != "vk-ns-demo-0" || src.snapshot == nil || src.snapshot.ID != "SNAP-1" {
		t.Errorf("source = %+v, want verified local snapshot", src)
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

	_, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0")
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

	_, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0")
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
	p.Registry = wakeVerifyRegistry{manifestErr: errors.New("registry down")}

	_, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0")
	if err == nil || !strings.Contains(err.Error(), "registry down") {
		t.Fatalf("err = %v, want fail-closed verification error", err)
	}
	if len(rt.snapshotRemoveCalls) != 0 {
		t.Errorf("unverifiable local must be kept, got removes %v", rt.snapshotRemoveCalls)
	}
}

// TestWakeStagesFromPeerAndClonesFromDir drives the no-local-snapshot wake:
// the peer's raw files land in a staging dir, clone receives --from-dir, and
// the staging dir is gone once the clone returns.
func TestWakeStagesFromPeerAndClonesFromDir(t *testing.T) {
	p, rt := newPeerWakeFixture(t, "SNAP-REMOTE")

	src, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0")
	if err != nil {
		t.Fatalf("resolveWakeSource: %v", err)
	}
	if src.dir == "" || src.localName != "" {
		t.Fatalf("source = %+v, want a staged dir", src)
	}
	if src.snapshot == nil || src.snapshot.ID != "SNAP-REMOTE" || src.snapshot.Name != "vk-ns-demo-0" {
		t.Fatalf("snapshot meta = %+v", src.snapshot)
	}
	if got, readErr := os.ReadFile(filepath.Join(src.dir, "memory-ranges")); readErr != nil || string(got) != "peer-mem-bytes" {
		t.Fatalf("staged memory-ranges = %q, err %v", got, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(src.dir, "snapshot.json")); statErr != nil {
		t.Fatalf("staged envelope missing: %v", statErr)
	}

	spec := meta.VMSpec{VMName: "vk-ns-demo-0", Network: "cocoon-dhcp"}
	if _, cloneErr := p.cloneFromHibernate(t.Context(), spec, src); cloneErr != nil {
		t.Fatalf("cloneFromHibernate: %v", cloneErr)
	}
	if rt.cloned == nil || rt.cloned.FromDir != src.dir || rt.cloned.From != "" {
		t.Fatalf("clone opts = %+v, want FromDir=%s", rt.cloned, src.dir)
	}
	if _, statErr := os.Stat(src.dir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("staging dir must be removed after clone, stat err = %v", statErr)
	}
}

// TestWakePeerMismatchFallsBackToPull proves the peer path is best-effort:
// a peer holding a different snapshot than the registry tag is rejected, and
// resolution falls through to the registry pull (surfaced here by nil Puller).
func TestWakePeerMismatchFallsBackToPull(t *testing.T) {
	p, _ := newPeerWakeFixture(t, "SNAP-REMOTE")
	// The registry moved on: same from-node stamp, newer snapshot ID; the
	// peer's copy no longer matches and must be rejected.
	reg := newWakeVerifyRegistry(t, "SNAP-NEWER")
	reg.manifestRaw = withManifestAnnotation(t, reg.manifestRaw, snapshots.AnnotationFromNode, "node-src")
	p.Registry = reg

	_, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0")
	if err == nil || !strings.Contains(err.Error(), "no puller configured") {
		t.Fatalf("err = %v, want fall-through to the registry pull path", err)
	}
}

// TestWakePeerUnreachableFallsBackToPull proves a dead peer never fails the
// wake outright.
func TestWakePeerUnreachableFallsBackToPull(t *testing.T) {
	p, _ := newPeerWakeFixture(t, "SNAP-REMOTE")
	p.PeerPort = "1" // nothing listens there

	_, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0")
	if err == nil || !strings.Contains(err.Error(), "no puller configured") {
		t.Fatalf("err = %v, want fall-through to the registry pull path", err)
	}
}

// TestWakePeerSelfNodeSkipsPeerPath: a from-node stamp naming this very node
// means tier-1 already missed (the local copy is gone or stale); fetching
// from ourselves would "succeed" and mask that, so it must be skipped. The
// fixture's peer server is live and holds the snapshot — without the skip,
// resolution would return it instead of falling through.
func TestWakePeerSelfNodeSkipsPeerPath(t *testing.T) {
	p, _ := newPeerWakeFixture(t, "SNAP-REMOTE")
	p.NodeName = "node-src" // the annotation now names this node itself

	_, err := p.resolveWakeSource(t.Context(), "vk-ns-demo-0")
	if err == nil || !strings.Contains(err.Error(), "no puller configured") {
		t.Fatalf("err = %v, want fall-through to the registry pull path", err)
	}
}

// wakeVerifyRegistry serves one hibernate-tag artifact (manifest + config
// blob) so resolveWakeSource's local-cache verification can run against it.
type wakeVerifyRegistry struct {
	fakeRegistry
	tagExists   bool
	manifestErr error
	manifestRaw []byte
	blobs       map[string][]byte
}

func newWakeVerifyRegistry(t *testing.T, snapshotID string) wakeVerifyRegistry {
	t.Helper()
	return newWakeVerifyRegistryWithImage(t, snapshotID, "")
}

func newWakeVerifyRegistryWithImage(t *testing.T, snapshotID, baseImage string) wakeVerifyRegistry {
	t.Helper()
	raw, blobs := hibernateArtifact(t, snapshotID, baseImage)
	return wakeVerifyRegistry{tagExists: true, manifestRaw: raw, blobs: blobs}
}

func (r wakeVerifyRegistry) GetManifest(context.Context, string, string) ([]byte, string, error) {
	if r.manifestErr != nil {
		return nil, "", r.manifestErr
	}
	if !r.tagExists {
		return nil, "", fmt.Errorf("get manifest: %w", snapshot.ErrManifestNotFound)
	}
	return r.manifestRaw, "", nil
}

func (r wakeVerifyRegistry) GetBlob(_ context.Context, _, digest string) (io.ReadCloser, error) {
	b, ok := r.blobs[digest]
	if !ok {
		return nil, errors.New("blob not found")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// withManifestAnnotation returns raw with one manifest annotation added, the
// way Pusher.amendFromNode stamps from-node post-push.
func withManifestAnnotation(t *testing.T, raw []byte, key, value string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	annotations, _ := m["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
	}
	annotations[key] = value
	m["annotations"] = annotations
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// hibernateArtifact builds the registry view of a pushed snapshot whose
// export came from the local snapshot with the given ID.
func hibernateArtifact(t *testing.T, snapshotID, baseImage string) ([]byte, map[string][]byte) {
	t.Helper()
	cfg := manifest.SnapshotConfig{SchemaVersion: "v1", SnapshotID: snapshotID}
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + ociutil.SHA256Hex(cfgBytes)
	memBytes := []byte("fake-memory-ranges")
	memDigest := "sha256:" + ociutil.SHA256Hex(memBytes)
	m := manifest.OCIManifest{
		SchemaVersion: 2,
		MediaType:     manifest.MediaTypeOCIManifest,
		ArtifactType:  manifest.ArtifactTypeSnapshot,
		Config: manifest.Descriptor{
			MediaType: manifest.MediaTypeSnapshotConfig,
			Digest:    digest,
			Size:      int64(len(cfgBytes)),
		},
		Layers: []manifest.Descriptor{{
			MediaType:   "application/octet-stream",
			Digest:      memDigest,
			Size:        int64(len(memBytes)),
			Annotations: map[string]string{manifest.AnnotationTitle: "memory-ranges"},
		}},
	}
	if baseImage != "" {
		m.Annotations = map[string]string{manifest.AnnotationSnapshotBaseImage: baseImage}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw, map[string][]byte{digest: cfgBytes, memDigest: memBytes}
}

// newPeerWakeFixture stands up a source node's peer server (store dir with
// one snapshot) plus a waking provider pointed at it via a fake k8s Node.
func newPeerWakeFixture(t *testing.T, snapshotID string) (*Provider, *fakeRuntime) {
	t.Helper()

	storeRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(storeRoot, snapshotID), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeRoot, snapshotID, "memory-ranges"), []byte("peer-mem-bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	sourceRT := &fakeRuntime{snapshots: map[string]*vm.Snapshot{
		"vk-ns-demo-0": {Name: "vk-ns-demo-0", ID: snapshotID},
	}}
	peerSrv := httptest.NewServer((&snapshots.PeerServer{Snapshots: sourceRT, StoreDir: storeRoot}).Handler())
	t.Cleanup(peerSrv.Close)
	peerURL, err := url.Parse(peerSrv.URL)
	if err != nil {
		t.Fatal(err)
	}

	rt := &fakeRuntime{}
	p := newTestProvider(t)
	p.Runtime = rt
	reg := newWakeVerifyRegistry(t, snapshotID)
	reg.manifestRaw = withManifestAnnotation(t, reg.manifestRaw, snapshots.AnnotationFromNode, "node-src")
	p.Registry = reg
	p.PeerRestorer = &snapshots.PeerRestorer{StagingRoot: t.TempDir()}
	p.PeerPort = peerURL.Port()
	p.Clientset = fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-src"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "127.0.0.1"},
		}},
	})
	return p, rt
}
