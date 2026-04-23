package snapshots

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/epoch/manifest"
)

const (
	cloudimgDownloadTimeout = 30 * time.Minute
	cloudimgMaxSize         = 20 << 30 // 20 GiB
)

// MirrorOCIImage copies an OCI image from a source registry to epoch.
// Streams layer blobs directly without local buffering.
func (p *Pusher) MirrorOCIImage(ctx context.Context, srcRef, dstRepo string) (string, error) {
	logger := log.WithFunc("snapshots.MirrorOCIImage")

	ref, err := name.ParseReference(srcRef)
	if err != nil {
		return "", fmt.Errorf("parse image ref %s: %w", srcRef, err)
	}

	desc, err := remote.Get(ref, remote.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("fetch image descriptor %s: %w", srcRef, err)
	}

	img, err := desc.Image()
	if err != nil {
		return "", fmt.Errorf("resolve image %s: %w", srcRef, err)
	}

	m, err := img.Manifest()
	if err != nil {
		return "", fmt.Errorf("read manifest %s: %w", srcRef, err)
	}

	layers, err := img.Layers()
	if err != nil {
		return "", fmt.Errorf("read layers %s: %w", srcRef, err)
	}
	for i, layer := range layers {
		dgst, err := layer.Digest()
		if err != nil {
			return "", fmt.Errorf("layer %d digest: %w", i, err)
		}
		digest := dgst.String()

		exists, err := p.Registry.BlobExists(ctx, dstRepo, digest)
		if err != nil {
			return "", fmt.Errorf("check blob %s: %w", digest, err)
		}
		if exists {
			continue
		}

		size, err := layer.Size()
		if err != nil {
			return "", fmt.Errorf("layer %d size: %w", i, err)
		}
		rc, err := layer.Compressed()
		if err != nil {
			return "", fmt.Errorf("open layer %d: %w", i, err)
		}
		putErr := p.Registry.PutBlob(ctx, dstRepo, digest, rc, size)
		rc.Close() //nolint:errcheck
		if putErr != nil {
			return "", fmt.Errorf("put layer %s: %w", digest, putErr)
		}
		logger.Debugf(ctx, "mirrored layer %s (%d bytes)", digest, size)
	}

	// Copy config blob.
	cfgDigest := m.Config.Digest.String()
	if exists, _ := p.Registry.BlobExists(ctx, dstRepo, cfgDigest); !exists {
		cfgData, err := img.RawConfigFile()
		if err != nil {
			return "", fmt.Errorf("read config: %w", err)
		}
		if err := p.Registry.PutBlob(ctx, dstRepo, cfgDigest, bytes.NewReader(cfgData), int64(len(cfgData))); err != nil {
			return "", fmt.Errorf("put config blob: %w", err)
		}
	}

	// Put manifest.
	rawManifest, err := img.RawManifest()
	if err != nil {
		return "", fmt.Errorf("raw manifest: %w", err)
	}
	tag := desc.Digest.String()
	if err := p.Registry.PutManifest(ctx, dstRepo, tag, rawManifest, string(m.MediaType)); err != nil {
		return "", fmt.Errorf("put manifest: %w", err)
	}

	logger.Infof(ctx, "mirrored OCI image %s → epoch/%s@%s", srcRef, dstRepo, tag)
	return tag, nil
}

// MirrorCloudimg downloads a cloudimg from URL and pushes it to epoch
// as an OCI artifact with ArtifactTypeOSImage format.
func (p *Pusher) MirrorCloudimg(ctx context.Context, srcURL, dstRepo, dstTag string) error {
	logger := log.WithFunc("snapshots.MirrorCloudimg")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	resp, err := (&http.Client{Timeout: cloudimgDownloadTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", srcURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", srcURL, resp.StatusCode)
	}

	// Need the digest before PutBlob, so buffer the body.
	hasher := sha256.New()
	data, err := io.ReadAll(io.TeeReader(io.LimitReader(resp.Body, cloudimgMaxSize), hasher))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	digest := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	blobSize := int64(len(data))

	if exists, _ := p.Registry.BlobExists(ctx, dstRepo, digest); !exists {
		if err := p.Registry.PutBlob(ctx, dstRepo, digest, bytes.NewReader(data), blobSize); err != nil {
			return fmt.Errorf("put blob: %w", err)
		}
		logger.Debugf(ctx, "uploaded cloudimg blob %s (%d bytes)", digest, blobSize)
	}
	data = nil // release memory

	// Empty OCI config.
	emptyConfig := []byte("{}")
	emptyDigest := "sha256:" + sha256Hex(emptyConfig)
	if exists, _ := p.Registry.BlobExists(ctx, dstRepo, emptyDigest); !exists {
		_ = p.Registry.PutBlob(ctx, dstRepo, emptyDigest, bytes.NewReader(emptyConfig), int64(len(emptyConfig)))
	}

	ociManifest := manifest.OCIManifest{
		SchemaVersion: 2,
		MediaType:     manifest.MediaTypeOCIManifest,
		ArtifactType:  manifest.ArtifactTypeOSImage,
		Config: manifest.Descriptor{
			MediaType: manifest.MediaTypeOCIEmpty,
			Digest:    emptyDigest,
			Size:      int64(len(emptyConfig)),
		},
		Layers: []manifest.Descriptor{{
			MediaType:   manifest.MediaTypeDiskQcow2,
			Digest:      digest,
			Size:        blobSize,
			Annotations: map[string]string{manifest.AnnotationTitle: "disk.qcow2"},
		}},
	}

	manifestBytes, err := json.Marshal(ociManifest)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := p.Registry.PutManifest(ctx, dstRepo, dstTag, manifestBytes, manifest.MediaTypeOCIManifest); err != nil {
		return fmt.Errorf("put manifest: %w", err)
	}

	logger.Infof(ctx, "mirrored cloudimg %s → epoch/%s:%s", srcURL, dstRepo, dstTag)
	return nil
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
