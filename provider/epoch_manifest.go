package provider

import (
	"archive/tar"
	"maps"
	"strings"
	"time"

	"github.com/cocoonstack/epoch/manifest"
)

// epochManifestDocument is the on-wire manifest JSON used by vk-cocoon.
// It extends Epoch's public manifest model with tar header metadata needed
// to reconstruct Cocoon's sparse snapshot archives losslessly.
type epochManifestDocument struct {
	manifest.Manifest
	// MediaType is the top-level manifest media type. Empty for cocoon-native
	// manifests; set to application/vnd.oci.* or application/vnd.docker.* for
	// OCI Distribution / Docker format manifests.
	MediaType    string                         `json:"mediaType,omitempty"`
	LayerHeaders map[string]snapshotLayerHeader `json:"layerHeaders,omitempty"`
}

// IsOCIImage reports whether the manifest is an OCI Distribution or Docker
// format manifest. Such manifests are pulled by cocoon's built-in
// go-containerregistry client — vk-cocoon skips its own snapshot pull path
// for them and lets cocoon resolve the image directly.
func (d *epochManifestDocument) IsOCIImage() bool {
	mt := strings.TrimSpace(d.MediaType)
	return strings.HasPrefix(mt, "application/vnd.oci") ||
		strings.HasPrefix(mt, "application/vnd.docker")
}

type snapshotLayerHeader struct {
	Mode       int64             `json:"mode,omitempty"`
	PAXRecords map[string]string `json:"paxRecords,omitempty"`
}

func snapshotLayerHeaderFromTarHeader(hdr *tar.Header) snapshotLayerHeader {
	meta := snapshotLayerHeader{Mode: hdr.Mode}
	if len(hdr.PAXRecords) > 0 {
		meta.PAXRecords = maps.Clone(hdr.PAXRecords)
	}
	return meta
}

func tarHeaderForLayer(layer manifest.Layer, meta snapshotLayerHeader, modTime time.Time) *tar.Header {
	mode := meta.Mode
	if mode == 0 {
		mode = 0o640
	}

	hdr := &tar.Header{
		Name:    layer.Filename,
		Size:    layer.Size,
		Mode:    mode,
		ModTime: modTime,
	}
	if len(meta.PAXRecords) > 0 {
		hdr.PAXRecords = maps.Clone(meta.PAXRecords)
	}
	return hdr
}
