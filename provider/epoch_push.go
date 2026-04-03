package provider

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cocoonstack/epoch/cocoon"
	"github.com/cocoonstack/epoch/manifest"
	"github.com/projecteru2/core/log"
)

// PushSnapshot uploads a local snapshot to the epoch registry via HTTP.
// It exports the snapshot via `cocoon snapshot export`, extracts the archive,
// uploads each file as a content-addressable blob, and builds a manifest.
func (p *EpochPuller) PushSnapshot(ctx context.Context, snapshotName, tag string) error {
	tag = cmp.Or(tag, "latest")
	log.WithFunc("provider.PushSnapshot").Infof(ctx, "[epoch] pushing %s:%s via HTTP...", snapshotName, tag)
	start := time.Now()

	cfg, dataDir, err := p.exportSnapshotForPush(ctx, snapshotName)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dataDir) //nolint:errcheck

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("read exported dir %s: %w", dataDir, err)
	}

	var layers []manifest.Layer
	var totalSize int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filePath := filepath.Join(dataDir, entry.Name())
		digest, size, blobErr := p.pushBlob(ctx, snapshotName, filePath)
		if blobErr != nil {
			return fmt.Errorf("push blob %s: %w", entry.Name(), blobErr)
		}
		layers = append(layers, manifest.Layer{
			Digest:   digest,
			Size:     size,
			Filename: entry.Name(),
		})
		totalSize += size
		log.WithFunc("provider.PushSnapshot").Infof(ctx, "[epoch]   %s -> sha256:%s (%s)", entry.Name(), digest[:12], cocoon.HumanSize(size))
	}

	// Base images from cloudimg blob dir (if referenced by snapshot).
	var (
		baseImages   []manifest.Layer
		imageBlobIDs = make(map[string]string)
	)
	if cfg.ImageBlobIDs != nil {
		blobDir := p.paths.CloudimgBlobDir()
		for hexID := range cfg.ImageBlobIDs {
			for _, ext := range []string{".qcow2", ".raw", ""} {
				fp := filepath.Join(blobDir, hexID+ext)
				if _, statErr := os.Stat(fp); statErr == nil {
					imageBlobIDs[hexID] = filepath.Base(fp)
					digest, size, blobErr := p.pushBlob(ctx, snapshotName, fp)
					if blobErr != nil {
						log.WithFunc("provider.PushSnapshot").Warnf(ctx, "[epoch]   skipping base image %s upload: %v", shortHex(hexID), blobErr)
						break
					}
					baseImages = append(baseImages, manifest.Layer{
						Digest:   digest,
						Size:     size,
						Filename: filepath.Base(fp),
					})
					totalSize += size
					break
				}
			}
		}
	}

	m := &manifest.Manifest{
		SchemaVersion: 1,
		Name:          snapshotName,
		Tag:           tag,
		SnapshotID:    cfg.ID,
		Image:         cfg.Image,
		ImageBlobIDs:  imageBlobIDs,
		CPU:           cfg.CPU,
		Memory:        cfg.Memory,
		Storage:       cfg.Storage,
		NICs:          cfg.NICs,
		Layers:        layers,
		BaseImages:    baseImages,
		TotalSize:     totalSize,
		PushedAt:      time.Now(),
	}

	if err := p.pushManifest(ctx, snapshotName, tag, m); err != nil {
		return fmt.Errorf("push manifest: %w", err)
	}

	log.WithFunc("provider.PushSnapshot").Infof(ctx, "[epoch] %s:%s pushed in %s (%s)", snapshotName, tag, time.Since(start).Round(time.Second), cocoon.HumanSize(totalSize))
	return nil
}

// pushBlob uploads a single file as a blob via PUT /v2/{name}/blobs/sha256:{digest}.
// Returns the SHA-256 digest and file size. Skips upload if blob already exists (HEAD check).
func (p *EpochPuller) pushBlob(ctx context.Context, name, filePath string) (string, int64, error) {
	f, err := os.Open(filePath) //nolint:gosec // filePath is from local snapshot data directory
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	_ = f.Close()
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", filePath, err)
	}
	digest := hex.EncodeToString(h.Sum(nil))

	headReq, headErr := http.NewRequestWithContext(ctx, http.MethodHead, p.blobURL(name, digest), nil)
	if headErr == nil {
		p.setAuth(headReq)
		if headResp, doErr := p.client.Do(headReq); doErr == nil { //nolint:gosec // registry endpoint comes from trusted snapshot registry configuration
			_ = headResp.Body.Close()
			if headResp.StatusCode == http.StatusOK {
				return digest, size, nil
			}
		}
	}

	f, err = os.Open(filePath) //nolint:gosec // filePath is from local snapshot data directory
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.blobURL(name, digest), f)
	if err != nil {
		return "", 0, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	p.setAuth(req)

	resp, err := p.client.Do(req) //nolint:gosec // epoch serverURL is configured by the trusted provider setup
	if err != nil {
		return "", 0, fmt.Errorf("put blob: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return "", 0, fmt.Errorf("put blob %s: %d %s", digest[:12], resp.StatusCode, readLimitedBody(resp.Body))
	}

	return digest, size, nil
}

// pushManifest uploads a manifest via PUT /v2/{name}/manifests/{tag}.
func (p *EpochPuller) pushManifest(ctx context.Context, name, tag string, m *manifest.Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.manifestURL(name, tag), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/vnd.epoch.manifest.v1+json")
	p.setAuth(req)

	resp, err := p.client.Do(req) //nolint:gosec // epoch serverURL is configured by the trusted provider setup
	if err != nil {
		return fmt.Errorf("put manifest: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("put manifest %s:%s: %d %s", name, tag, resp.StatusCode, readLimitedBody(resp.Body))
	}
	return nil
}

// DeleteSnapshot removes a snapshot's manifest from epoch (blobs are left for GC).
func (p *EpochPuller) DeleteSnapshot(ctx context.Context, name, tag string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, p.tagDeleteURL(name, tag), nil)
	if err != nil {
		return err
	}
	p.setAuth(req)
	resp, err := p.client.Do(req) //nolint:gosec // epoch serverURL is configured by the trusted provider setup
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}
