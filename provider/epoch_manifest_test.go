package provider

import (
	"testing"

	"github.com/cocoonstack/epoch/manifest"
)

func TestEpochManifestIsCloudImage(t *testing.T) {
	t.Run("direct cloud image", func(t *testing.T) {
		m := &manifest.Manifest{
			Image: "win1125h2-latest.qcow2",
			Layers: []manifest.Layer{
				{Filename: "win1125h2-latest.qcow2.part.001"},
				{Filename: "win1125h2-latest.qcow2.part.002"},
			},
		}
		if !m.IsCloudImage() {
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
		if m.IsCloudImage() {
			t.Fatal("snapshot manifest misclassified as cloud image")
		}
	})
}
