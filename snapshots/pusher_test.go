package snapshots

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/cocoonstack/cocoon-common/manifest"
	"github.com/cocoonstack/cocoon-common/oci"
	"github.com/cocoonstack/cocoon-common/snapshot"
	"github.com/cocoonstack/vk-cocoon/vm"
)

func TestPushSnapshotWaitsOnTheGateForSpoolPushes(t *testing.T) {
	if err := pushGate.Acquire(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	defer pushGate.Release(1)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := (&Pusher{}).PushSnapshot(ctx, "vm-a", "", "", "", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a v1 spool push with the gate held returned %v, want the gate wait to expire", err)
	}
}

func TestPushSnapshotStampsCallerAnnotationsBesideTheNodeHint(t *testing.T) {
	reg := &manifestRecorder{}
	p := &Pusher{Registry: reg, Runtime: exportRuntime{export: exportTar(t, "vm-a")}, NodeName: "node-b"}
	caller := map[string]string{"ai.simular.owner": "org:osworld", AnnotationFromNode: "spoofed"}
	if err := p.PushSnapshot(t.Context(), "vm-a", "", "hibernate", "img:1", caller); err != nil {
		t.Fatalf("push: %v", err)
	}
	var m struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(reg.manifest, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if got := m.Annotations["ai.simular.owner"]; got != "org:osworld" {
		t.Errorf("caller annotation = %q, want org:osworld", got)
	}
	if got := m.Annotations[AnnotationFromNode]; got != "node-b" {
		t.Errorf("%s = %q, want the pushing node", AnnotationFromNode, got)
	}
	if got := m.Annotations[manifest.AnnotationSnapshotBaseImage]; got != "img:1" {
		t.Errorf("%s = %q, want img:1", manifest.AnnotationSnapshotBaseImage, got)
	}
	if caller[AnnotationFromNode] != "spoofed" {
		t.Errorf("push mutated the caller's map")
	}
}

type manifestRecorder struct {
	oci.Registry
	manifest []byte
}

func (r *manifestRecorder) HasBlob(context.Context, string, string) (bool, error) { return false, nil }

func (r *manifestRecorder) PutBlob(_ context.Context, _, _ string, body io.ReadSeeker, _ int64) error {
	_, err := io.Copy(io.Discard, body)
	return err
}

func (r *manifestRecorder) PutManifest(_ context.Context, _, _ string, data []byte, _ string) error {
	r.manifest = data
	return nil
}

type exportRuntime struct {
	vm.Runtime
	export []byte
}

func (r exportRuntime) SnapshotExport(context.Context, string) (io.ReadCloser, func() error, error) {
	return io.NopCloser(bytes.NewReader(r.export)), func() error { return nil }, nil
}

func exportTar(t *testing.T, name string) []byte {
	t.Helper()
	envelope, err := snapshot.MarshalEnvelope(&manifest.SnapshotConfig{SchemaVersion: "v1", SnapshotID: "SNAP-1"}, name)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "snapshot.json", Mode: 0o644, Size: int64(len(envelope))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(envelope); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
