package snapshots

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/cocoon-common/manifest"

	"github.com/cocoonstack/vk-cocoon/vm"
)

type stubResolver map[string]*vm.Snapshot

func (s stubResolver) Snapshot(_ context.Context, name string) (*vm.Snapshot, error) {
	if snap, ok := s[name]; ok {
		return snap, nil
	}
	return nil, fmt.Errorf("inspect: %w", vm.ErrSnapshotNotFound)
}

func randomContent(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// newPeerFixture builds a source store with one snapshot dir and serves it.
func newPeerFixture(t *testing.T, snapshotID string, files map[string][]byte) (*httptest.Server, *PeerRestorer) {
	t.Helper()
	storeRoot := t.TempDir()
	dir := filepath.Join(storeRoot, snapshotID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer((&PeerServer{
		Snapshots: stubResolver{"vm-a": {Name: "vm-a", ID: snapshotID}},
		StoreDir:  storeRoot,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, &PeerRestorer{StagingRoot: t.TempDir()}
}

func testConfig(snapshotID string) *manifest.SnapshotConfig {
	return &manifest.SnapshotConfig{
		SnapshotID: snapshotID,
		Image:      "ghcr.io/example/base:1",
		Hypervisor: "cloud-hypervisor",
		CPU:        2,
		Memory:     1 << 30,
	}
}

func TestPeerRestoreRoundtrip(t *testing.T) {
	files := map[string][]byte{
		"memory-ranges": randomContent(t, 3<<20+123),
		"overlay.qcow2": randomContent(t, 1<<20),
		"state.json":    []byte(`{"s":1}`),
	}
	srv, r := newPeerFixture(t, "SNAP-1", files)

	res, cleanup, err := r.Restore(t.Context(), srv.URL, "vm-a", "vm-a", testConfig("SNAP-1"))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for name, content := range files {
		got, readErr := os.ReadFile(filepath.Join(res.Dir, name))
		if readErr != nil || !bytes.Equal(got, content) {
			t.Errorf("staged %s: %d bytes, err %v", name, len(got), readErr)
		}
	}
	env, err := os.ReadFile(filepath.Join(res.Dir, "snapshot.json"))
	if err != nil || !strings.Contains(string(env), `"id": "SNAP-1"`) || !strings.Contains(string(env), `"name": "vm-a"`) {
		t.Errorf("envelope = %s, err %v", env, err)
	}
	if res.Snapshot.ID != "SNAP-1" || res.Snapshot.Image != "ghcr.io/example/base:1" {
		t.Errorf("snapshot meta = %+v", res.Snapshot)
	}

	cleanup()
	if _, err := os.Stat(res.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("cleanup must remove staging dir, stat err = %v", err)
	}
}

func TestPeerRestoreLargeFileMultiSlice(t *testing.T) {
	// Force multiple slices without moving peerSliceBytes: serve a plan whose
	// slices are hand-split, via a real server (extents already ≤ cap) — here
	// we just assert splitExtents produces the pieces fetchFiles consumes.
	got := splitExtents([]extent{{offset: 0, length: 10 << 20}}, 4<<20)
	want := []peerSlice{{0, 4 << 20}, {4 << 20, 4 << 20}, {8 << 20, 2 << 20}}
	if len(got) != len(want) {
		t.Fatalf("splitExtents = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitExtents[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPeerRestoreRejectsIDMismatch(t *testing.T) {
	srv, r := newPeerFixture(t, "SNAP-OLD", map[string][]byte{"f": []byte("x")})

	_, _, err := r.Restore(t.Context(), srv.URL, "vm-a", "vm-a", testConfig("SNAP-CURRENT"))
	if err == nil || !strings.Contains(err.Error(), "registry says") {
		t.Fatalf("err = %v, want snapshot ID mismatch", err)
	}
	entries, _ := os.ReadDir(r.StagingRoot)
	if len(entries) != 0 {
		t.Errorf("rejected restore must leave no staging dirs, found %d", len(entries))
	}
}

func TestPeerRestoreChecksumMismatchFails(t *testing.T) {
	srv, r := newPeerFixture(t, "SNAP-1", map[string][]byte{"memory-ranges": randomContent(t, 1<<20)})
	// A corrupting proxy: flip a byte in every slice body, keep the plan intact.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		resp, err := http.Get(srv.URL + req.URL.String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close() //nolint:errcheck
		if strings.HasPrefix(req.URL.Path, slicePath) {
			buf := new(bytes.Buffer)
			if _, err := buf.ReadFrom(resp.Body); err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			b := buf.Bytes()
			b[0] ^= 0xff
			w.Header().Set("Trailer", sliceChecksumTrailer)
			w.WriteHeader(http.StatusOK)
			w.Write(b) //nolint:errcheck,gosec
			w.Header().Set(sliceChecksumTrailer, resp.Trailer.Get(sliceChecksumTrailer))
			return
		}
		w.WriteHeader(resp.StatusCode)
		if _, err := buf2buf(w, resp); err != nil {
			return
		}
	}))
	t.Cleanup(proxy.Close)

	_, _, err := r.Restore(t.Context(), proxy.URL, "vm-a", "vm-a", testConfig("SNAP-1"))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
	entries, _ := os.ReadDir(r.StagingRoot)
	if len(entries) != 0 {
		t.Errorf("failed restore must remove its staging dir, found %d", len(entries))
	}
}

func buf2buf(w http.ResponseWriter, resp *http.Response) (int64, error) {
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return 0, err
	}
	n, err := w.Write(buf.Bytes())
	return int64(n), err
}

func TestPeerServerRejectsTraversal(t *testing.T) {
	srv, _ := newPeerFixture(t, "SNAP-1", map[string][]byte{"f": []byte("x")})

	for _, path := range []string{
		slicePath + "?id=..&file=f&offset=0&length=1",
		slicePath + "?id=SNAP-1&file=..%2Ff&offset=0&length=1",
		planPath + "?name=no-such-vm",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck,gosec
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s: status %d, want a rejection", path, resp.StatusCode)
		}
	}
}

func TestSweepStaging(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "vm-old-123", "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vm-old-123", "memory-ranges"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	SweepStaging(t.Context(), root)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("sweep left %d entries", len(entries))
	}
	SweepStaging(t.Context(), filepath.Join(root, "does-not-exist")) // must not panic
}

func TestPeerRestoreSparseSourceFile(t *testing.T) {
	storeRoot := t.TempDir()
	dir := filepath.Join(storeRoot, "SNAP-SP")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Data at [0,64K) and [3M,3M+32K); hole between; apparent size 4M.
	f, err := os.Create(filepath.Join(dir, "overlay.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	segA, segB := randomContent(t, 64<<10), randomContent(t, 32<<10)
	if _, err := f.WriteAt(segA, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(segB, 3<<20); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(4 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close() //nolint:errcheck,gosec

	srv := httptest.NewServer((&PeerServer{
		Snapshots: stubResolver{"vm-sp": {Name: "vm-sp", ID: "SNAP-SP"}},
		StoreDir:  storeRoot,
	}).Handler())
	t.Cleanup(srv.Close)
	r := &PeerRestorer{StagingRoot: t.TempDir()}

	res, cleanup, err := r.Restore(t.Context(), srv.URL, "vm-sp", "vm-sp", testConfig("SNAP-SP"))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	defer cleanup()
	// Whether extents came from SEEK_DATA or the dense fallback, the restored
	// bytes must be identical: data in place, hole read back as zeros.
	got, err := os.ReadFile(filepath.Join(res.Dir, "overlay.qcow2"))
	if err != nil || len(got) != 4<<20 {
		t.Fatalf("restored size = %d, err %v", len(got), err)
	}
	if !bytes.Equal(got[:len(segA)], segA) || !bytes.Equal(got[3<<20:3<<20+len(segB)], segB) {
		t.Error("data segments mismatch")
	}
	for i, c := range got[len(segA) : 3<<20] {
		if c != 0 {
			t.Fatalf("hole byte %d = %#x, want zero", len(segA)+i, c)
		}
	}
}

func TestCoalesceExtents(t *testing.T) {
	got := coalesceExtents([]extent{
		{0, 100}, {200, 100}, // gap 100 ≤ maxGap → merge
		{10 << 20, 100}, {20 << 20, 100}, // gap ~10MB > maxGap → keep split
	}, 4<<20)
	want := []extent{{0, 300}, {10 << 20, 100}, {20 << 20, 100}}
	if len(got) != len(want) {
		t.Fatalf("coalesceExtents = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("coalesceExtents[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestWriteSkippingZerosPreservesContentAndHoles(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	if err := f.Truncate(6 * zeroSkipBytes); err != nil {
		t.Fatal(err)
	}

	// Chunk layout: [data][zeros][data+partial tail] — zeros must be skipped,
	// data must land at its offsets.
	data := make([]byte, 5*zeroSkipBytes+123)
	copy(data[0:], bytes.Repeat([]byte{0xAA}, zeroSkipBytes))
	copy(data[3*zeroSkipBytes:], bytes.Repeat([]byte{0xBB}, zeroSkipBytes))
	data[5*zeroSkipBytes+100] = 0xCC

	if err := writeSkippingZeros(f, data, 0); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 0xAA || got[zeroSkipBytes-1] != 0xAA {
		t.Error("first data chunk mismatch")
	}
	for i := zeroSkipBytes; i < 3*zeroSkipBytes; i++ {
		if got[i] != 0 {
			t.Fatalf("zero region dirtied at %d", i)
		}
	}
	if got[3*zeroSkipBytes] != 0xBB || got[5*zeroSkipBytes+100] != 0xCC {
		t.Error("later data chunks mismatch")
	}
}
