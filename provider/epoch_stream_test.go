package provider

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocoonstack/epoch/manifest"
)

const (
	testPAXSparseMap  = "COCOON.sparse.map"
	testPAXSparseSize = "COCOON.sparse.size"
)

func TestSnapshotStreamRoundTripPreservesSparseMetadata(t *testing.T) {
	ctx := context.Background()
	exportStream := makeSnapshotExportStream(t, true)

	blobs := make(map[string][]byte)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		digest := strings.TrimPrefix(filepath.Base(r.URL.Path), "sha256:")
		switch r.Method {
		case http.MethodHead:
			if _, ok := blobs[digest]; ok {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			blobs[digest] = append([]byte(nil), data...)
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			data, ok := blobs[digest]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if _, err := w.Write(data); err != nil {
				t.Errorf("write get body: %v", err)
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	p := &EpochPuller{
		serverURL: server.URL,
		client:    server.Client(),
		pulled:    make(map[string]bool),
	}

	cfg, layers, layerHeaders, err := p.readAndUploadTarEntries(ctx, "demo", bytes.NewReader(exportStream))
	if err != nil {
		t.Fatalf("readAndUploadTarEntries: %v", err)
	}
	meta := layerHeaders["overlay.qcow2"]
	if meta.Mode != 0o600 {
		t.Fatalf("layer mode = %#o, want 0o600", meta.Mode)
	}
	if got := meta.PAXRecords[testPAXSparseMap]; got == "" {
		t.Fatal("sparse pax map missing")
	}
	if got := meta.PAXRecords[testPAXSparseSize]; got != "8192" {
		t.Fatalf("sparse pax size = %q, want 8192", got)
	}

	doc := &epochManifestDocument{
		Manifest: manifest.Manifest{
			SchemaVersion: 1,
			Name:          "demo",
			Tag:           "latest",
			SnapshotID:    cfg.ID,
			Image:         cfg.Image,
			CPU:           cfg.CPU,
			Memory:        cfg.Memory,
			Storage:       cfg.Storage,
			NICs:          cfg.NICs,
			Layers:        layers,
		},
		LayerHeaders: layerHeaders,
	}

	var rebuilt bytes.Buffer
	if err := p.writeSnapshotStream(ctx, "demo", doc, &rebuilt); err != nil {
		t.Fatalf("writeSnapshotStream: %v", err)
	}

	tr := tar.NewReader(bytes.NewReader(rebuilt.Bytes()))

	if hdr, err := tr.Next(); err != nil {
		t.Fatalf("read snapshot header: %v", err)
	} else if hdr.Name != snapshotJSONName {
		t.Fatalf("first tar entry = %q, want %q", hdr.Name, snapshotJSONName)
	}

	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("read sparse header: %v", err)
	}
	if hdr.Name != "overlay.qcow2" {
		t.Fatalf("overlay name = %q, want overlay.qcow2", hdr.Name)
	}
	if hdr.Mode != 0o600 {
		t.Fatalf("overlay mode = %#o, want 0o600", hdr.Mode)
	}
	if got := hdr.PAXRecords[testPAXSparseMap]; got != `[{"o":4096,"l":4}]` {
		t.Fatalf("overlay sparse map = %q", got)
	}
	if got := hdr.PAXRecords[testPAXSparseSize]; got != "8192" {
		t.Fatalf("overlay sparse size = %q, want 8192", got)
	}
	payload, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("read sparse payload: %v", err)
	}
	if got := string(payload); got != "DATA" {
		t.Fatalf("overlay payload = %q, want DATA", got)
	}
}

func TestReadAndUploadTarEntriesRejectsCorruptTar(t *testing.T) {
	p := &EpochPuller{}
	_, _, _, err := p.readAndUploadTarEntries(context.Background(), "demo", bytes.NewReader([]byte("not a tar archive")))
	if err == nil {
		t.Fatal("expected tar read error")
	}
}

func TestCopyBlobUsesGoHTTPClientWithTokenAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v2/demo/blobs/sha256:abc123" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		if _, err := io.WriteString(w, "blob-data"); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()

	p := &EpochPuller{
		serverURL: server.URL,
		token:     "test-token",
		client:    server.Client(),
	}

	var buf bytes.Buffer
	if err := p.copyBlob(context.Background(), "demo", "abc123", &buf); err != nil {
		t.Fatalf("copyBlob: %v", err)
	}
	if got := buf.String(); got != "blob-data" {
		t.Fatalf("blob data = %q, want blob-data", got)
	}
}

func makeSnapshotExportStream(t *testing.T, includeSparseLayer bool) []byte {
	t.Helper()

	envelope := snapshotExportEnvelope{
		Version: 1,
		Config: snapshotExportConfig{
			ID:      "snap-1",
			Name:    "demo",
			Image:   "ubuntu:24.04",
			CPU:     2,
			Memory:  4 << 30,
			Storage: 15 << 30,
			NICs:    1,
		},
	}
	jsonData, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		t.Fatalf("marshal snapshot metadata: %v", err)
	}
	jsonData = append(jsonData, '\n')

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	if err := tw.WriteHeader(&tar.Header{
		Name: snapshotJSONName,
		Mode: 0o644,
		Size: int64(len(jsonData)),
	}); err != nil {
		t.Fatalf("write snapshot header: %v", err)
	}
	if _, err := tw.Write(jsonData); err != nil {
		t.Fatalf("write snapshot metadata: %v", err)
	}

	if includeSparseLayer {
		sparseMap := `[{"o":4096,"l":4}]`
		if err := tw.WriteHeader(&tar.Header{
			Name: "overlay.qcow2",
			Mode: 0o600,
			Size: 4,
			PAXRecords: map[string]string{
				testPAXSparseMap:  sparseMap,
				testPAXSparseSize: "8192",
			},
		}); err != nil {
			t.Fatalf("write sparse header: %v", err)
		}
		if _, err := tw.Write([]byte("DATA")); err != nil {
			t.Fatalf("write sparse payload: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	return buf.Bytes()
}
