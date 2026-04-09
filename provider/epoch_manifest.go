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
// to reconstruct Cocoon's sparse snapshot archives losslessly, and a
// top-level MediaType field so OCI / Docker manifests can be detected
// without parsing the full envelope.
type epochManifestDocument struct {
	manifest.Manifest
	// MediaType is the manifest's top-level mediaType field. Empty for the
	// cocoon-native epoch format; populated for OCI Distribution and Docker
	// manifests pushed via the OCI flow.
	MediaType    string                         `json:"mediaType,omitempty"`
	LayerHeaders map[string]snapshotLayerHeader `json:"layerHeaders,omitempty"`
}

// ociManifestMediaTypePrefixes lists the top-level manifest mediaType
// prefixes that mean "OCI / Docker image — let cocoon pull it directly".
// Intentionally narrow: `vnd.oci.image.` would also match config and layer
// types (e.g. `vnd.oci.image.config.v1+json`) which never appear as a
// top-level manifest mediaType but conceptually do not belong here.
var ociManifestMediaTypePrefixes = []string{
	"application/vnd.oci.image.manifest.",
	"application/vnd.oci.image.index.",
	"application/vnd.oci.artifact.manifest.",
	"application/vnd.docker.distribution.manifest.",
}

// IsOCIImage reports whether the manifest is in an OCI Distribution or
// Docker format. Cocoon's CH backend pulls these directly via its built-in
// go-containerregistry client, so vk-cocoon must skip its own snapshot
// import path and let the runtime handle the resolution.
func (d *epochManifestDocument) IsOCIImage() bool {
	mt := strings.TrimSpace(d.MediaType)
	for _, p := range ociManifestMediaTypePrefixes {
		if strings.HasPrefix(mt, p) {
			return true
		}
	}
	return false
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
