package snapshots

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
)

func TestAmendFromNodePreservesUnknownFields(t *testing.T) {
	// A manifest with an existing annotation and a field this code does not
	// model; both must survive the amend byte-for-byte in content.
	reg := &amendRegistry{manifestRaw: []byte(`{
		"schemaVersion": 2,
		"artifactType": "application/vnd.cocoonstack.snapshot.v2+json",
		"annotations": {"cocoonstack.snapshot.baseimage": "img:1"},
		"futureField": {"nested": [1, 2, 3]}
	}`)}
	p := &Pusher{Registry: reg, NodeName: "node-7"}

	if err := p.amendFromNode(t.Context(), "vm-a", "hibernate"); err != nil {
		t.Fatalf("amendFromNode: %v", err)
	}
	if reg.putTag != "hibernate" {
		t.Errorf("put tag = %q", reg.putTag)
	}
	var m map[string]any
	if err := json.Unmarshal(reg.putRaw, &m); err != nil {
		t.Fatal(err)
	}
	annotations := m["annotations"].(map[string]any)
	if annotations[AnnotationFromNode] != "node-7" {
		t.Errorf("from-node = %v", annotations[AnnotationFromNode])
	}
	if annotations["cocoonstack.snapshot.baseimage"] != "img:1" {
		t.Errorf("pre-existing annotation lost: %v", annotations)
	}
	if _, ok := m["futureField"]; !ok {
		t.Error("unknown field dropped by the amend round-trip")
	}
}

// amendRegistry captures the PutManifest an amend performs.
type amendRegistry struct {
	manifestRaw []byte
	putRaw      []byte
	putTag      string
}

func (r *amendRegistry) GetManifest(context.Context, string, string) ([]byte, string, error) {
	return r.manifestRaw, "", nil
}

func (r *amendRegistry) GetBlob(context.Context, string, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}
func (r *amendRegistry) HasBlob(context.Context, string, string) (bool, error) { return false, nil }
func (r *amendRegistry) PutBlob(context.Context, string, string, io.Reader, int64) error {
	return nil
}

func (r *amendRegistry) PutManifest(_ context.Context, _, tag string, data []byte, _ string) error {
	r.putTag, r.putRaw = tag, data
	return nil
}

func (r *amendRegistry) HasManifest(context.Context, string, string) (bool, error) {
	return true, nil
}
func (r *amendRegistry) DeleteManifest(context.Context, string, string) error { return nil }
