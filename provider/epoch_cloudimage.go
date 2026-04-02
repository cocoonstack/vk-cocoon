package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	if !isCloudImageManifest(m) {
		return fmt.Errorf("manifest %s:%s is not a direct cloud image", name, tag)
	}

	if err := p.importCloudImage(ctx, name, m); err != nil {
		return fmt.Errorf("import cloud image %s:%s: %w", name, tag, err)
	}

	p.markPulled(ref)
	return nil
}

func (p *EpochPuller) ensureLocalSnapshotMetadata(name string) (bool, error) {
	db, err := p.paths.ReadSnapshotDB()
	if err != nil {
		return false, err
	}
	sid, ok := db.Names[name]
	if !ok {
		return false, nil
	}
	rec := db.Snapshots[sid]
	if rec == nil {
		return false, nil
	}
	if err := hydrateSnapshotRecord(rec); err != nil {
		return false, err
	}
	return true, p.paths.WriteSnapshotDB(db)
}

func (p *EpochPuller) updateSnapshotDB(m *manifest.Manifest, name, dataDir string) error {
	db, err := p.paths.ReadSnapshotDB()
	if err != nil {
		return err
	}

	blobIDs := make(map[string]struct{})
	for _, bi := range m.BaseImages {
		blobIDs[trimBlobExt(bi.Filename)] = struct{}{}
	}
	for id, filename := range m.ImageBlobIDs {
		if id != "" {
			blobIDs[id] = struct{}{}
			continue
		}
		if filename != "" {
			blobIDs[trimBlobExt(filename)] = struct{}{}
		}
	}

	rec := &cocoon.SnapshotRecord{
		ID:           m.SnapshotID,
		Name:         name,
		Image:        m.Image,
		ImageBlobIDs: blobIDs,
		CPU:          m.CPU,
		Memory:       m.Memory,
		Storage:      m.Storage,
		NICs:         m.NICs,
		CreatedAt:    m.CreatedAt,
		DataDir:      dataDir,
	}
	if err := hydrateSnapshotRecord(rec); err != nil {
		return fmt.Errorf("hydrate snapshot metadata: %w", err)
	}
	db.Snapshots[m.SnapshotID] = rec
	db.Names[name] = m.SnapshotID

	return p.paths.WriteSnapshotDB(db)
}

// isCloudImageManifest returns true if the manifest describes a direct cloud
// image (disk layers only, no snapshot config or base images).
func isCloudImageManifest(m *manifest.Manifest) bool {
	if m == nil || len(m.Layers) == 0 {
		return false
	}
	if len(m.BaseImages) > 0 || len(m.ImageBlobIDs) > 0 {
		return false
	}
	hasConfig := false
	hasDiskLayer := false
	for _, layer := range m.Layers {
		switch layer.Filename {
		case "config.json", "state.json", "memory-ranges", "cidata.img":
			hasConfig = true
		}
		if strings.HasSuffix(layer.Filename, ".qcow2") || strings.Contains(layer.Filename, ".qcow2.part.") ||
			strings.HasSuffix(layer.Filename, ".raw") || strings.Contains(layer.Filename, ".raw.part.") {
			hasDiskLayer = true
		}
	}
	return !hasConfig && hasDiskLayer
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
		if err := p.downloadBlob(ctx, name, bi.Digest, destPath); err != nil {
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
		return fmt.Errorf("GET source image %s: %d %s", imageURL, resp.StatusCode, readLimitedBody(resp.Body))
	}

	tmpPath := destPath + ".tmp"
	f, err := os.Create(tmpPath) //nolint:gosec // destPath is constructed from trusted config
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return err
	}
	gotHex := hex.EncodeToString(h.Sum(nil))
	if expectedHex != "" && !strings.EqualFold(gotHex, expectedHex) {
		return fmt.Errorf("source image digest mismatch: got %s want %s", gotHex, expectedHex)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, destPath)
}

type cloudImageIndex struct {
	Images map[string]*cloudImageEntry `json:"images"`
}

type cloudImageEntry struct {
	Ref string `json:"ref"`
}

type chSnapshotConfig struct {
	CPUs struct {
		BootVCPUs int `json:"boot_vcpus"`
	} `json:"cpus"`
	Memory struct {
		Size int64 `json:"size"`
	} `json:"memory"`
	Nets []json.RawMessage `json:"net"`
}

func hydrateSnapshotRecord(rec *cocoon.SnapshotRecord) error {
	if rec == nil || rec.DataDir == "" {
		return nil
	}
	configPath := filepath.Join(rec.DataDir, "config.json")
	data, err := os.ReadFile(configPath) //nolint:gosec // configPath is from local snapshot data directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var cfg chSnapshotConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", configPath, err)
	}
	if rec.CPU == 0 {
		rec.CPU = cfg.CPUs.BootVCPUs
	}
	if rec.Memory == 0 {
		rec.Memory = cfg.Memory.Size
	}
	if rec.NICs == 0 {
		rec.NICs = len(cfg.Nets)
	}
	return nil
}

func (p *EpochPuller) importCloudImage(ctx context.Context, name string, m *manifest.Manifest) error {
	dataDir := p.paths.SnapshotDataDir(m.SnapshotID)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dataDir, err)
	}

	partFiles := make([]string, 0, len(m.Layers))
	for _, layer := range m.Layers {
		destPath := filepath.Join(dataDir, layer.Filename)
		partFiles = append(partFiles, destPath)
		if _, err := os.Stat(destPath); err == nil {
			continue
		}
		log.WithFunc("provider.importCloudImage").Infof(ctx, "[epoch]   downloading %s (%s)...", layer.Filename, cocoon.HumanSize(layer.Size))
		if err := p.downloadBlob(ctx, name, layer.Digest, destPath); err != nil {
			return fmt.Errorf("download %s: %w", layer.Filename, err)
		}
	}

	args := []string{"image", "import", name}
	for _, part := range partFiles {
		args = append(args, "--file", part)
	}
	if out, err := p.cocoonExec(ctx, args...); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out))
	}

	if err := p.removeSnapshotRecord(name, m.SnapshotID); err != nil {
		return fmt.Errorf("cleanup stale snapshot db entry: %w", err)
	}
	if err := os.RemoveAll(dataDir); err != nil {
		return fmt.Errorf("remove import cache %s: %w", dataDir, err)
	}
	return nil
}

func (p *EpochPuller) removeSnapshotRecord(name, snapshotID string) error {
	db, err := p.paths.ReadSnapshotDB()
	if err != nil {
		return err
	}
	if existingID, ok := db.Names[name]; ok && (snapshotID == "" || existingID == snapshotID) {
		delete(db.Names, name)
		delete(db.Snapshots, existingID)
		return p.paths.WriteSnapshotDB(db)
	}
	return nil
}

func (p *EpochPuller) cloudImageExists(ctx context.Context, name string) bool {
	imageDB := filepath.Join(p.rootDir, "cloudimg", "db", "images.json")
	data, err := os.ReadFile(imageDB) //nolint:gosec // imageDB is from trusted config path
	if err == nil {
		if index, jsonErr := decodeJSON[cloudImageIndex](strings.NewReader(string(data))); jsonErr == nil {
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

func trimBlobExt(filename string) string {
	for _, ext := range []string{".qcow2", ".raw"} {
		if trimmed, ok := strings.CutSuffix(filename, ext); ok {
			return trimmed
		}
	}
	return filename
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
