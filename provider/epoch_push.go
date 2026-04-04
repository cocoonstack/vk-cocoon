package provider

import (
	"archive/tar"
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
// It streams `cocoon snapshot export -o -` stdout directly, reading tar entries
// and uploading each as a content-addressable blob.
func (p *EpochPuller) PushSnapshot(ctx context.Context, snapshotName, tag string) error {
	tag = cmp.Or(tag, "latest")
	logger := log.WithFunc("provider.PushSnapshot")
	logger.Infof(ctx, "[epoch] pushing %s:%s via HTTP...", snapshotName, tag)
	start := time.Now()

	cfg, layers, layerHeaders, err := p.exportAndUploadBlobs(ctx, snapshotName)
	if err != nil {
		return err
	}

	var totalSize int64
	for _, l := range layers {
		totalSize += l.Size
	}

	// Upload base images from cloudimg blob dir (if referenced by snapshot).
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
						logger.Warnf(ctx, "[epoch]   skipping base image %s upload: %v", shortHex(hexID), blobErr)
						break
					}
					baseImages = append(baseImages, manifest.Layer{
						Digest:    digest,
						Size:      size,
						Filename:  filepath.Base(fp),
						MediaType: manifest.MediaTypeForFile(filepath.Base(fp)),
					})
					totalSize += size
					break
				}
			}
		}
	}

	doc := &epochManifestDocument{
		Manifest: manifest.Manifest{
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
		},
		LayerHeaders: layerHeaders,
	}

	if err := p.pushManifest(ctx, snapshotName, tag, doc); err != nil {
		return fmt.Errorf("push manifest: %w", err)
	}

	logger.Infof(ctx, "[epoch] %s:%s pushed in %s (%s)", snapshotName, tag, time.Since(start).Round(time.Second), cocoon.HumanSize(totalSize))
	return nil
}

// exportAndUploadBlobs streams `cocoon snapshot export -o -` and uploads each
// tar entry as a blob. Returns the parsed snapshot config and layer list.
func (p *EpochPuller) exportAndUploadBlobs(ctx context.Context, name string) (*snapshotExportConfig, []manifest.Layer, map[string]snapshotLayerHeader, error) {
	cmd := p.buildCocoonCmd(ctx, "snapshot", "export", name, "-o", "-")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("start cocoon snapshot export: %w", err)
	}

	cfg, layers, layerHeaders, readErr := p.readAndUploadTarEntries(ctx, name, stdout)
	waitErr := cmd.Wait()
	if readErr != nil {
		if waitErr != nil {
			return nil, nil, nil, fmt.Errorf("read stream: %w, export: %w", readErr, waitErr)
		}
		return nil, nil, nil, readErr
	}
	if waitErr != nil {
		return nil, nil, nil, fmt.Errorf("cocoon snapshot export: %w", waitErr)
	}
	return cfg, layers, layerHeaders, nil
}

// readAndUploadTarEntries reads a tar stream, parses snapshot.json,
// and uploads each data file as a blob via temp file + hash.
func (p *EpochPuller) readAndUploadTarEntries(ctx context.Context, name string, r io.Reader) (*snapshotExportConfig, []manifest.Layer, map[string]snapshotLayerHeader, error) {
	logger := log.WithFunc("provider.readAndUploadTarEntries")
	tr := tar.NewReader(r)
	var (
		cfg          *snapshotExportConfig
		layers       []manifest.Layer
		layerHeaders = make(map[string]snapshotLayerHeader)
	)

	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, nil, nil, fmt.Errorf("read tar entry: %w", nextErr)
		}

		if hdr.Name == snapshotJSONName {
			var envelope snapshotExportEnvelope
			if decErr := json.NewDecoder(tr).Decode(&envelope); decErr != nil {
				return nil, nil, nil, fmt.Errorf("parse snapshot.json: %w", decErr)
			}
			cfg = &envelope.Config
			continue
		}

		// Write entry to temp file while computing hash, then upload.
		digest, size, uploadErr := p.uploadTarEntry(ctx, name, hdr.Name, tr, hdr.Size)
		if uploadErr != nil {
			return nil, nil, nil, fmt.Errorf("upload %s: %w", hdr.Name, uploadErr)
		}
		layers = append(layers, manifest.Layer{
			Digest:    digest,
			Size:      size,
			Filename:  hdr.Name,
			MediaType: manifest.MediaTypeForFile(hdr.Name),
		})
		layerHeaders[hdr.Name] = snapshotLayerHeaderFromTarHeader(hdr)
		logger.Infof(ctx, "[epoch]   %s -> sha256:%s (%s)", hdr.Name, shortHex(digest), cocoon.HumanSize(size))
	}

	if cfg == nil {
		return nil, nil, nil, fmt.Errorf("snapshot.json not found in export stream")
	}
	return cfg, layers, layerHeaders, nil
}

// uploadTarEntry writes a tar entry to a temp file while hashing, then uploads the blob.
func (p *EpochPuller) uploadTarEntry(ctx context.Context, repoName, filename string, r io.Reader, size int64) (string, int64, error) {
	tmpFile, err := os.CreateTemp("", "epoch-blob-*")
	if err != nil {
		return "", 0, fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name()) //nolint:errcheck
	defer tmpFile.Close()           //nolint:errcheck

	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmpFile, h), io.LimitReader(r, size))
	if err != nil {
		return "", 0, fmt.Errorf("buffer %s: %w", filename, err)
	}

	digest := hex.EncodeToString(h.Sum(nil))

	if p.blobExists(ctx, repoName, digest) {
		return digest, written, nil
	}

	if _, seekErr := tmpFile.Seek(0, io.SeekStart); seekErr != nil {
		return "", 0, fmt.Errorf("seek temp file: %w", seekErr)
	}

	if err := p.putBlob(ctx, repoName, digest, tmpFile, written); err != nil {
		return "", 0, err
	}
	return digest, written, nil
}

func (p *EpochPuller) blobExists(ctx context.Context, name, digest string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, p.blobURL(name, digest), nil)
	if err != nil {
		return false
	}
	p.setAuth(req)
	resp, err := p.client.Do(req) //nolint:gosec // epoch serverURL is configured by the trusted provider setup
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// pushBlob uploads a single local file as a blob (used for base images).
func (p *EpochPuller) pushBlob(ctx context.Context, name, filePath string) (string, int64, error) {
	f, err := os.Open(filePath) //nolint:gosec // filePath is from local snapshot data directory
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", filePath, err)
	}
	digest := hex.EncodeToString(h.Sum(nil))

	if p.blobExists(ctx, name, digest) {
		return digest, size, nil
	}

	if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
		return "", 0, fmt.Errorf("seek %s: %w", filePath, seekErr)
	}

	if err := p.putBlob(ctx, name, digest, f, size); err != nil {
		return "", 0, err
	}
	return digest, size, nil
}

// pushManifest uploads a manifest via PUT /v2/{name}/manifests/{tag}.
func (p *EpochPuller) pushManifest(ctx context.Context, name, tag string, doc *epochManifestDocument) error {
	data, err := json.MarshalIndent(doc, "", "  ")
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
