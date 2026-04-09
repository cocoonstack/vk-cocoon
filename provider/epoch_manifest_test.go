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

func TestEpochManifestDocumentIsOCIImage(t *testing.T) {
	tests := []struct {
		name      string
		mediaType string
		want      bool
	}{
		{name: "OCI image manifest", mediaType: "application/vnd.oci.image.manifest.v1+json", want: true},
		{name: "OCI image index", mediaType: "application/vnd.oci.image.index.v1+json", want: true},
		{name: "Docker manifest v2", mediaType: "application/vnd.docker.distribution.manifest.v2+json", want: true},
		{name: "Docker manifest list", mediaType: "application/vnd.docker.distribution.manifest.list.v2+json", want: true},
		{name: "epoch native (no mediaType)", mediaType: "", want: false},
		{name: "epoch explicit mediaType", mediaType: "application/vnd.epoch.manifest.v1+json", want: false},
		{name: "cocoon snapshot", mediaType: "application/vnd.cocoon.snapshot.v1+json", want: false},
		{name: "leading whitespace tolerated", mediaType: "  application/vnd.oci.image.manifest.v1+json", want: true},
		{name: "OCI artifact manifest", mediaType: "application/vnd.oci.artifact.manifest.v1+json", want: true},
		// Negative near-misses: types that contain `oci` or `docker` but
		// are not top-level manifest mediaTypes.
		{name: "OCI image config (not a manifest)", mediaType: "application/vnd.oci.image.config.v1+json", want: false},
		{name: "OCI image layer (not a manifest)", mediaType: "application/vnd.oci.image.layer.v1.tar+gzip", want: false},
		{name: "Docker container config (not a manifest)", mediaType: "application/vnd.docker.container.image.v1+json", want: false},
		{name: "Docker rootfs layer (not a manifest)", mediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip", want: false},
		{name: "Helm chart config", mediaType: "application/vnd.cncf.helm.config.v1+json", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := &epochManifestDocument{MediaType: tt.mediaType}
			if got := doc.IsOCIImage(); got != tt.want {
				t.Errorf("IsOCIImage(%q) = %v, want %v", tt.mediaType, got, tt.want)
			}
		})
	}
}
