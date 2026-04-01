package provider

import (
	"os"
	"path/filepath"
	"testing"

	cocoon "github.com/cocoonstack/epoch/cocoon"
	"github.com/cocoonstack/epoch/manifest"
)

func TestEpochManifestIsCloudImageManifest(t *testing.T) {
	t.Run("direct cloud image", func(t *testing.T) {
		m := &manifest.Manifest{
			Image: "win1125h2-latest.qcow2",
			Layers: []manifest.Layer{
				{Filename: "win1125h2-latest.qcow2.part.001"},
				{Filename: "win1125h2-latest.qcow2.part.002"},
			},
		}
		if !isCloudImageManifest(m) {
			t.Fatal("expected direct cloud image manifest")
		}
	})

	t.Run("snapshot payload", func(t *testing.T) {
		m := &manifest.Manifest{
			Name: "ubuntu-dev-base",
			Layers: []manifest.Layer{
				{Filename: "config.json"},
				{Filename: "overlay.qcow2"},
			},
		}
		if isCloudImageManifest(m) {
			t.Fatal("snapshot manifest misclassified as cloud image")
		}
	})
}

func TestHydrateSnapshotRecord(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configJSON := `{
	  "cpus": {"boot_vcpus": 2},
	  "memory": {"size": 8589934592},
	  "net": [{"tap":"tap0"}]
	}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	rec := &cocoon.SnapshotRecord{
		Name:    "ubuntu-dev-base",
		DataDir: dir,
	}
	if err := hydrateSnapshotRecord(rec); err != nil {
		t.Fatalf("hydrateSnapshotRecord: %v", err)
	}
	if rec.CPU != 2 {
		t.Fatalf("cpu mismatch: got %d want 2", rec.CPU)
	}
	if rec.Memory != 8589934592 {
		t.Fatalf("memory mismatch: got %d", rec.Memory)
	}
	if rec.NICs != 1 {
		t.Fatalf("nics mismatch: got %d want 1", rec.NICs)
	}
}
