package provider

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocoonstack/epoch/cocoon"
	"github.com/cocoonstack/epoch/manifest"
	"github.com/projecteru2/core/log"
)

// EnsureCloudImage ensures a direct cloud image manifest is imported into the
// local cocoon image store under its repository name.
func (p *EpochPuller) EnsureCloudImage(ctx context.Context, name string) error {
	return p.EnsureCloudImageTag(ctx, name, "latest")
}

// EnsureCloudImageTag ensures a specific cloud image tag is available locally.
func (p *EpochPuller) EnsureCloudImageTag(ctx context.Context, name, tag string) error {
	if p.ensureCloudImageTagFn != nil {
		return p.ensureCloudImageTagFn(ctx, name, tag)
	}

	ref := name + ":" + tag + "#cloudimg"
	if p.cachedPull(ref) {
		return nil
	}
	if p.cloudImageExists(ctx, name) {
		p.markPulled(ref)
		return nil
	}

	m, err := p.getManifest(ctx, name, tag)
	if err != nil {
		return fmt.Errorf("get manifest: %w", err)
	}
	if !m.IsCloudImage() {
		return fmt.Errorf("manifest %s:%s is not a direct cloud image", name, tag)
	}

	if err := p.importCloudImage(ctx, name, &m.Manifest); err != nil {
		return fmt.Errorf("import cloud image %s:%s: %w", name, tag, err)
	}

	p.markPulled(ref)
	return nil
}

func (p *EpochPuller) localSnapshotExists(ctx context.Context, name string) bool {
	_, err := p.cocoonExec(ctx, "snapshot", "inspect", name)
	return err == nil
}

func (p *EpochPuller) downloadBaseImages(ctx context.Context, name string, baseImages []manifest.Layer) error {
	blobDir := p.paths.CloudimgBlobDir()
	if err := os.MkdirAll(blobDir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", blobDir, err)
	}
	for _, bi := range baseImages {
		destPath := filepath.Join(blobDir, bi.Filename)
		if _, err := os.Stat(destPath); err == nil {
			continue
		}
		log.WithFunc("provider.downloadBaseImages").Infof(ctx, "[epoch]   downloading base image %s...", bi.Filename)
		if err := p.downloadBlobToFile(ctx, name, bi.Digest, destPath); err != nil {
			return fmt.Errorf("download base %s: %w", bi.Filename, err)
		}
		_ = os.Chmod(destPath, 0o444) //nolint:gosec // read-only base images
	}
	return nil
}

func (p *EpochPuller) downloadBaseImagesFromSource(ctx context.Context, m *manifest.Manifest) error {
	blobDir := p.paths.CloudimgBlobDir()
	if err := os.MkdirAll(blobDir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", blobDir, err)
	}
	for hexID, filename := range m.ImageBlobIDs {
		if hexID == "" {
			continue
		}
		baseName := filename
		if baseName == "" {
			baseName = hexID + ".qcow2"
		}
		destPath := filepath.Join(blobDir, baseName)
		if _, err := os.Stat(destPath); err == nil {
			continue
		}
		log.WithFunc("provider.downloadBaseImagesFromSource").Infof(ctx, "[epoch]   downloading base image source %s -> %s...", m.Image, baseName)
		if err := p.downloadSourceImage(ctx, m.Image, hexID, destPath); err != nil {
			return fmt.Errorf("download base source %s: %w", m.Image, err)
		}
		_ = os.Chmod(destPath, 0o444) //nolint:gosec // read-only base images
	}
	return nil
}

func (p *EpochPuller) downloadSourceImage(ctx context.Context, imageURL, expectedHex, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req) //nolint:gosec // source image URL comes from trusted snapshot manifest metadata
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get source image %s: %d %s", imageURL, resp.StatusCode, readLimitedBody(resp.Body))
	}

	tmpPath := destPath + ".tmp"
	f, err := os.Create(tmpPath) //nolint:gosec // destPath is constructed from trusted config
	if err != nil {
		return err
	}

	h := sha256.New()
	if _, copyErr := io.Copy(io.MultiWriter(f, h), resp.Body); copyErr != nil {
		f.Close()          //nolint:errcheck,gosec
		os.Remove(tmpPath) //nolint:errcheck,gosec
		return copyErr
	}
	gotHex := hex.EncodeToString(h.Sum(nil))
	if expectedHex != "" && !strings.EqualFold(gotHex, expectedHex) {
		f.Close()          //nolint:errcheck,gosec
		os.Remove(tmpPath) //nolint:errcheck,gosec
		return fmt.Errorf("source image digest mismatch: got %s want %s", gotHex, expectedHex)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath) //nolint:errcheck,gosec
		return err
	}
	return os.Rename(tmpPath, destPath)
}

// importCloudImage streams cloud image blobs directly into cocoon image import via pipe.
// Flow: gzip(concat(blobs)) → cocoon image import <name> stdin.
func (p *EpochPuller) importCloudImage(ctx context.Context, name string, m *manifest.Manifest) error {
	if err := p.pipeToImport(ctx, []string{"image", "import", name}, func(w io.Writer) error {
		return p.writeCloudImageStream(ctx, name, m, w)
	}); err != nil {
		return err
	}

	// Unregister any stale snapshot with the same name (may not exist).
	_, _ = p.cocoonExec(ctx, "snapshot", "rm", name)

	log.WithFunc("provider.importCloudImage").Infof(ctx, "[epoch] cloud image %s imported via stream", name)
	return nil
}

func (p *EpochPuller) writeCloudImageStream(ctx context.Context, name string, m *manifest.Manifest, w io.Writer) error {
	logger := log.WithFunc("provider.writeCloudImageStream")
	bw := bufio.NewWriterSize(w, 256<<10)
	gw, err := gzip.NewWriterLevel(bw, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("create gzip writer: %w", err)
	}

	for _, layer := range m.Layers {
		logger.Infof(ctx, "[epoch]   streaming %s (%s)...", layer.Filename, cocoon.HumanSize(layer.Size))
		if err := p.copyBlob(ctx, name, layer.Digest, gw); err != nil {
			return fmt.Errorf("stream %s: %w", layer.Filename, err)
		}
	}

	if err := gw.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}
	return bw.Flush()
}

// downloadBlobToFile downloads a blob from the registry to a local file (used for base images).
func (p *EpochPuller) downloadBlobToFile(ctx context.Context, name, digest, destPath string) error {
	f, err := os.Create(destPath) //nolint:gosec // destPath is constructed from trusted config
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	return p.copyBlob(ctx, name, digest, f)
}

type cloudImageIndex struct {
	Images map[string]*cloudImageEntry `json:"images"`
}

type cloudImageEntry struct {
	Ref string `json:"ref"`
}

func (p *EpochPuller) cloudImageExists(ctx context.Context, name string) bool {
	imageDB := filepath.Join(p.rootDir, "cloudimg", "db", "images.json")
	data, err := os.ReadFile(imageDB) //nolint:gosec // imageDB is from trusted config path
	if err == nil {
		if index, jsonErr := decodeJSON[cloudImageIndex](bytes.NewReader(data)); jsonErr == nil {
			if entry := index.Images[name]; entry != nil {
				return true
			}
		}
	}
	if p.cocoonBin == "" {
		return false
	}
	_, err = p.cocoonExec(ctx, "image", "inspect", name)
	return err == nil
}

func shortHex(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func isHTTPURL(raw string) bool {
	return strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
}
