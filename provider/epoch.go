// HTTP-based Epoch client for vk-cocoon.
//
// Handles both pull (download) and push (upload) of snapshots via the Epoch
// registry server's V2 API. This removes any direct object-store dependency
// from the provider.
//
// Usage:
//
//	puller := NewEpochPuller("http://epoch-server:8080", "/data01/cocoon")
//	puller.EnsureSnapshot(ctx, "ubuntu-dev-base")         // pull
//	puller.PushSnapshot(ctx, "vk-default-demo-0", "latest") // push
package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// EpochPuller pulls snapshots from the Epoch HTTP registry.
type EpochPuller struct {
	serverURL string
	rootDir   string
	cocoonBin string
	token     string // Bearer token for registry auth (empty = no auth)
	client    *http.Client

	mu     sync.Mutex
	pulled map[string]bool
}

// NewEpochPuller creates a puller that talks to the Epoch HTTP server.
// If EPOCH_REGISTRY_TOKEN is set, it is sent as a Bearer token on every request.
func NewEpochPuller(serverURL, rootDir, cocoonBin string) *EpochPuller {
	if strings.TrimSpace(cocoonBin) == "" {
		cocoonBin = "cocoon"
	}
	return &EpochPuller{
		serverURL: serverURL,
		rootDir:   rootDir,
		cocoonBin: cocoonBin,
		token:     os.Getenv("EPOCH_REGISTRY_TOKEN"),
		client: &http.Client{
			// No global timeout — large blob downloads can take minutes.
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		pulled: make(map[string]bool),
	}
}

// setAuth adds Bearer token to an HTTP request if configured.
func (p *EpochPuller) setAuth(req *http.Request) {
	if token := p.authToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func (p *EpochPuller) authToken() string {
	if token := strings.TrimSpace(os.Getenv("EPOCH_REGISTRY_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(p.token)
}

// RootDir returns the cocoon data root directory.
func (p *EpochPuller) RootDir() string { return p.rootDir }

// EnsureSnapshot ensures a snapshot is available locally, pulling via HTTP if needed.
func (p *EpochPuller) EnsureSnapshot(ctx context.Context, name string) error {
	return p.EnsureSnapshotTag(ctx, name, "latest")
}

// EnsureSnapshotTag ensures a specific tag is available locally.
func (p *EpochPuller) EnsureSnapshotTag(ctx context.Context, name, tag string) error {
	ref := name + ":" + tag

	p.mu.Lock()
	if p.pulled[ref] {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	// Check if already exists locally.
	if ok, err := p.ensureLocalSnapshotMetadata(name); err != nil {
		return fmt.Errorf("repair local snapshot %s: %w", name, err)
	} else if ok {
		p.mu.Lock()
		p.pulled[ref] = true
		p.mu.Unlock()
		return nil
	}

	log.Printf("[epoch] pulling %s via HTTP...", ref)
	start := time.Now()

	if err := p.pull(ctx, name, tag); err != nil {
		return fmt.Errorf("epoch HTTP pull %s: %w", ref, err)
	}

	log.Printf("[epoch] %s pulled in %s", ref, time.Since(start).Round(time.Second))
	p.mu.Lock()
	p.pulled[ref] = true
	p.mu.Unlock()
	return nil
}

func (p *EpochPuller) pull(ctx context.Context, name, tag string) error {
	// 1. Get manifest.
	m, err := p.getManifest(ctx, name, tag)
	if err != nil {
		return fmt.Errorf("get manifest: %w", err)
	}
	if m.isCloudImageManifest() {
		return fmt.Errorf("manifest %s:%s is a cloud image, not a snapshot", name, tag)
	}

	sid := m.SnapshotID
	dataDir := filepath.Join(p.rootDir, "snapshot", "localfile", sid)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dataDir, err)
	}

	// 2. Download layers.
	for _, layer := range m.Layers {
		destPath := filepath.Join(dataDir, layer.Filename)
		if _, err := os.Stat(destPath); err == nil {
			log.Printf("[epoch]   %s exists, skip", layer.Filename)
			continue
		}
		log.Printf("[epoch]   downloading %s (%s)...", layer.Filename, humanSize(layer.Size))
		if err := p.downloadBlob(ctx, name, layer.Digest, destPath); err != nil {
			return fmt.Errorf("download %s: %w", layer.Filename, err)
		}
	}

	// 3. Ensure base images.
	switch {
	case len(m.BaseImages) > 0:
		if err := p.downloadBaseImages(ctx, name, m.BaseImages); err != nil {
			return err
		}
	case len(m.ImageBlobIDs) > 0 && isHTTPURL(m.Image):
		if err := p.downloadBaseImagesFromSource(ctx, m); err != nil {
			return err
		}
	}

	// 4. Update snapshots.json.
	return p.updateSnapshotDB(m, name, dataDir)
}

// EnsureCloudImage ensures a direct cloud image manifest is imported into the
// local cocoon image store under its repository name.
func (p *EpochPuller) EnsureCloudImage(ctx context.Context, name string) error {
	return p.EnsureCloudImageTag(ctx, name, "latest")
}

func (p *EpochPuller) EnsureCloudImageTag(ctx context.Context, name, tag string) error {
	ref := name + ":" + tag + "#cloudimg"

	p.mu.Lock()
	if p.pulled[ref] {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	if p.cloudImageExists(ctx, name) {
		p.mu.Lock()
		p.pulled[ref] = true
		p.mu.Unlock()
		return nil
	}

	m, err := p.getManifest(ctx, name, tag)
	if err != nil {
		return fmt.Errorf("get manifest: %w", err)
	}
	if !m.isCloudImageManifest() {
		return fmt.Errorf("manifest %s:%s is not a direct cloud image", name, tag)
	}

	if err := p.importCloudImage(ctx, name, m); err != nil {
		return fmt.Errorf("import cloud image %s:%s: %w", name, tag, err)
	}

	p.mu.Lock()
	p.pulled[ref] = true
	p.mu.Unlock()
	return nil
}

// --- HTTP calls ---

func (p *EpochPuller) getManifest(ctx context.Context, name, tag string) (*epochManifest, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", p.serverURL, name, tag)
	if p.authToken() != "" {
		return p.getManifestWithCurl(ctx, url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	p.setAuth(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET manifest %s:%s: %d %s", name, tag, resp.StatusCode, string(body))
	}

	var m epochManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

func (p *EpochPuller) downloadBlob(ctx context.Context, name, digest, destPath string) error {
	url := fmt.Sprintf("%s/v2/%s/blobs/sha256:%s", p.serverURL, name, digest)
	if p.authToken() != "" {
		return p.downloadBlobWithCurl(ctx, url, destPath)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	p.setAuth(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET blob %s: %d %s", digest[:12], resp.StatusCode, string(body))
	}

	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

// Epoch's public gateway intermittently rejects token-authenticated requests
// from Go's client stack while equivalent curl requests succeed on the same host.
// When a registry token is configured, use curl for the whole pull path so
// manifest/blob requests stay on the same known-good client behavior.
func (p *EpochPuller) getManifestWithCurl(ctx context.Context, url string) (*epochManifest, error) {
	data, err := p.curlRead(ctx, url)
	if err != nil {
		return nil, err
	}
	var m epochManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode manifest via curl: %w", err)
	}
	return &m, nil
}

func (p *EpochPuller) downloadBlobWithCurl(ctx context.Context, url, destPath string) error {
	tmp := destPath + ".part"
	if err := p.curlWriteFile(ctx, url, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, destPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (p *EpochPuller) curlRead(ctx context.Context, url string) ([]byte, error) {
	args := []string{"-fsSL", "--retry", "3"}
	if token := p.authToken(); token != "" {
		args = append(args, "-H", "Authorization: Bearer "+token)
	}
	args = append(args, url)
	cmd := exec.CommandContext(ctx, "curl", args...) //nolint:gosec
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return nil, fmt.Errorf("curl GET %s: %s", url, strings.TrimSpace(string(ee.Stderr)))
	}
	return nil, fmt.Errorf("curl GET %s: %w", url, err)
}

func (p *EpochPuller) curlWriteFile(ctx context.Context, url, destPath string) error {
	args := []string{"-fsSL", "--retry", "3", "-o", destPath}
	if token := p.authToken(); token != "" {
		args = append(args, "-H", "Authorization: Bearer "+token)
	}
	args = append(args, url)
	cmd := exec.CommandContext(ctx, "curl", args...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	return fmt.Errorf("curl GET %s: %s", url, strings.TrimSpace(string(out)))
}

// --- Snapshot DB ---

func (p *EpochPuller) ensureLocalSnapshotMetadata(name string) (bool, error) {
	db, err := p.readSnapshotDB()
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
	return true, p.writeSnapshotDB(db)
}

func (p *EpochPuller) updateSnapshotDB(m *epochManifest, name, dataDir string) error {
	db, err := p.readSnapshotDB()
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

	rec := &snapshotRecord{
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

	return p.writeSnapshotDB(db)
}

func (p *EpochPuller) snapshotDBFile() string {
	return filepath.Join(p.rootDir, "snapshot", "db", "snapshots.json")
}

func (p *EpochPuller) readSnapshotDB() (*snapshotDB, error) {
	data, err := os.ReadFile(p.snapshotDBFile())
	if err != nil {
		if os.IsNotExist(err) {
			return &snapshotDB{
				Snapshots: make(map[string]*snapshotRecord),
				Names:     make(map[string]string),
			}, nil
		}
		return nil, err
	}
	var db snapshotDB
	if err := json.Unmarshal(data, &db); err != nil {
		return nil, err
	}
	if db.Snapshots == nil {
		db.Snapshots = make(map[string]*snapshotRecord)
	}
	if db.Names == nil {
		db.Names = make(map[string]string)
	}
	return &db, nil
}

func (p *EpochPuller) writeSnapshotDB(db *snapshotDB) error {
	data, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(p.snapshotDBFile())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp := p.snapshotDBFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p.snapshotDBFile())
}

// --- Types (minimal, matching epoch manifest format) ---

type epochManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	Name          string            `json:"name"`
	Tag           string            `json:"tag"`
	SnapshotID    string            `json:"snapshotId"`
	Image         string            `json:"image,omitempty"`
	ImageBlobIDs  map[string]string `json:"imageBlobIDs,omitempty"`
	CPU           int               `json:"cpu,omitempty"`
	Memory        int64             `json:"memory,omitempty"`
	Storage       int64             `json:"storage,omitempty"`
	NICs          int               `json:"nics,omitempty"`
	Layers        []epochLayer      `json:"layers"`
	BaseImages    []epochLayer      `json:"baseImages,omitempty"`
	TotalSize     int64             `json:"totalSize"`
	CreatedAt     time.Time         `json:"createdAt"`
	PushedAt      time.Time         `json:"pushedAt"`
}

func (m *epochManifest) isCloudImageManifest() bool {
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

func (p *EpochPuller) downloadBaseImages(ctx context.Context, name string, baseImages []epochLayer) error {
	blobDir := filepath.Join(p.rootDir, "cloudimg", "blobs")
	if err := os.MkdirAll(blobDir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", blobDir, err)
	}
	for _, bi := range baseImages {
		destPath := filepath.Join(blobDir, bi.Filename)
		if _, err := os.Stat(destPath); err == nil {
			continue
		}
		log.Printf("[epoch]   downloading base image %s...", bi.Filename)
		if err := p.downloadBlob(ctx, name, bi.Digest, destPath); err != nil {
			return fmt.Errorf("download base %s: %w", bi.Filename, err)
		}
		_ = os.Chmod(destPath, 0o444)
	}
	return nil
}

func (p *EpochPuller) downloadBaseImagesFromSource(ctx context.Context, m *epochManifest) error {
	blobDir := filepath.Join(p.rootDir, "cloudimg", "blobs")
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
		log.Printf("[epoch]   downloading base image source %s -> %s...", m.Image, baseName)
		if err := p.downloadSourceImage(ctx, m.Image, hexID, destPath); err != nil {
			return fmt.Errorf("download base source %s: %w", m.Image, err)
		}
		_ = os.Chmod(destPath, 0o444)
	}
	return nil
}

func (p *EpochPuller) downloadSourceImage(ctx context.Context, imageURL, expectedHex, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET source image %s: %d %s", imageURL, resp.StatusCode, string(body))
	}

	tmpPath := destPath + ".tmp"
	f, err := os.Create(tmpPath)
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

func trimBlobExt(filename string) string {
	for _, ext := range []string{".qcow2", ".raw"} {
		if strings.HasSuffix(filename, ext) {
			return strings.TrimSuffix(filename, ext)
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

type epochLayer struct {
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Filename  string `json:"filename"`
	MediaType string `json:"mediaType,omitempty"`
}

type snapshotDB struct {
	Snapshots map[string]*snapshotRecord `json:"snapshots"`
	Names     map[string]string          `json:"names"`
}

type snapshotRecord struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Image        string              `json:"image,omitempty"`
	ImageBlobIDs map[string]struct{} `json:"image_blob_ids,omitempty"`
	CPU          int                 `json:"cpu,omitempty"`
	Memory       int64               `json:"memory,omitempty"`
	Storage      int64               `json:"storage,omitempty"`
	NICs         int                 `json:"nics,omitempty"`
	CreatedAt    time.Time           `json:"created_at"`
	Pending      bool                `json:"pending,omitempty"`
	DataDir      string              `json:"data_dir,omitempty"`
}

// ---------- Push (upload to epoch) ----------

// PushSnapshot uploads a local snapshot to the epoch registry via HTTP.
// It reads the snapshot files from the local data directory, uploads each
// as a content-addressable blob, builds a manifest, and uploads it.
func (p *EpochPuller) PushSnapshot(ctx context.Context, snapshotName, tag string) error {
	if tag == "" {
		tag = "latest"
	}
	log.Printf("[epoch] pushing %s:%s via HTTP...", snapshotName, tag)
	start := time.Now()

	db, err := p.readSnapshotDB()
	if err != nil {
		return fmt.Errorf("read snapshot DB: %w", err)
	}

	sid, ok := db.Names[snapshotName]
	if !ok {
		return fmt.Errorf("snapshot %q not found in local DB", snapshotName)
	}
	rec := db.Snapshots[sid]
	if rec == nil {
		return fmt.Errorf("snapshot record %s not found", sid)
	}

	dataDir := rec.DataDir
	if dataDir == "" {
		dataDir = filepath.Join(p.rootDir, "snapshot", "localfile", sid)
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("read snapshot dir %s: %w", dataDir, err)
	}

	// Upload each file as a blob.
	var layers []epochLayer
	var totalSize int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filePath := filepath.Join(dataDir, entry.Name())
		digest, size, err := p.pushBlob(ctx, snapshotName, filePath)
		if err != nil {
			return fmt.Errorf("push blob %s: %w", entry.Name(), err)
		}
		layers = append(layers, epochLayer{
			Digest:   digest,
			Size:     size,
			Filename: entry.Name(),
		})
		totalSize += size
		log.Printf("[epoch]   %s → sha256:%s (%s)", entry.Name(), digest[:12], humanSize(size))
	}

	// Upload base images when the registry accepts them. Always record their
	// filenames in the manifest so pullers can reconstruct cloudimg blobs from
	// rec.Image if direct base-image upload is unavailable.
	var (
		baseImages   []epochLayer
		imageBlobIDs = make(map[string]string)
	)
	if rec.ImageBlobIDs != nil {
		blobDir := filepath.Join(p.rootDir, "cloudimg", "blobs")
		for hexID := range rec.ImageBlobIDs {
			for _, ext := range []string{".qcow2", ".raw", ""} {
				fp := filepath.Join(blobDir, hexID+ext)
				if _, err := os.Stat(fp); err == nil {
					imageBlobIDs[hexID] = filepath.Base(fp)
					digest, size, err := p.pushBlob(ctx, snapshotName, fp)
					if err != nil {
						log.Printf("[epoch]   skipping base image %s upload: %v", shortHex(hexID), err)
						break
					}
					baseImages = append(baseImages, epochLayer{
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

	m := &epochManifest{
		SchemaVersion: 1,
		Name:          snapshotName,
		Tag:           tag,
		SnapshotID:    sid,
		Image:         rec.Image,
		ImageBlobIDs:  imageBlobIDs,
		CPU:           rec.CPU,
		Memory:        rec.Memory,
		Storage:       rec.Storage,
		NICs:          rec.NICs,
		Layers:        layers,
		BaseImages:    baseImages,
		TotalSize:     totalSize,
		CreatedAt:     rec.CreatedAt,
		PushedAt:      time.Now(),
	}

	if err := p.pushManifest(ctx, snapshotName, tag, m); err != nil {
		return fmt.Errorf("push manifest: %w", err)
	}

	log.Printf("[epoch] %s:%s pushed in %s (%s)", snapshotName, tag, time.Since(start).Round(time.Second), humanSize(totalSize))
	return nil
}

// pushBlob uploads a single file as a blob via PUT /v2/{name}/blobs/sha256:{digest}.
// Returns the SHA-256 digest and file size. Skips upload if blob already exists (HEAD check).
func (p *EpochPuller) pushBlob(ctx context.Context, name, filePath string) (string, int64, error) {
	// Compute SHA-256.
	f, err := os.Open(filePath)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	size, err := io.Copy(h, f)
	f.Close()
	if err != nil {
		return "", 0, fmt.Errorf("hash %s: %w", filePath, err)
	}
	digest := hex.EncodeToString(h.Sum(nil))

	// HEAD check — skip if already uploaded.
	headURL := fmt.Sprintf("%s/v2/%s/blobs/sha256:%s", p.serverURL, name, digest)
	if headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, headURL, nil); err == nil {
		p.setAuth(headReq)
		if resp, err := p.client.Do(headReq); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return digest, size, nil
			}
		}
	}

	// Upload.
	f, err = os.Open(filePath)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	url := fmt.Sprintf("%s/v2/%s/blobs/sha256:%s", p.serverURL, name, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
	if err != nil {
		return "", 0, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	p.setAuth(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("PUT blob: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", 0, fmt.Errorf("PUT blob %s: %d %s", digest[:12], resp.StatusCode, string(body))
	}

	return digest, size, nil
}

// pushManifest uploads a manifest via PUT /v2/{name}/manifests/{tag}.
func (p *EpochPuller) pushManifest(ctx context.Context, name, tag string, m *epochManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	url := fmt.Sprintf("%s/v2/%s/manifests/%s", p.serverURL, name, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", "application/vnd.epoch.manifest.v1+json")
	p.setAuth(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("PUT manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PUT manifest %s:%s: %d %s", name, tag, resp.StatusCode, string(body))
	}
	return nil
}

// DeleteSnapshot removes a snapshot's manifest from epoch (blobs are left for GC).
func (p *EpochPuller) DeleteSnapshot(ctx context.Context, name, tag string) error {
	url := fmt.Sprintf("%s/api/repositories/%s/tags/%s", p.serverURL, name, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	p.setAuth(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func humanSize(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%dB", b)
	}
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

func hydrateSnapshotRecord(rec *snapshotRecord) error {
	if rec == nil || rec.DataDir == "" {
		return nil
	}
	configPath := filepath.Join(rec.DataDir, "config.json")
	data, err := os.ReadFile(configPath)
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

func (p *EpochPuller) importCloudImage(ctx context.Context, name string, m *epochManifest) error {
	dataDir := filepath.Join(p.rootDir, "snapshot", "localfile", m.SnapshotID)
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
		log.Printf("[epoch]   downloading %s (%s)...", layer.Filename, humanSize(layer.Size))
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
	db, err := p.readSnapshotDB()
	if err != nil {
		return err
	}
	if existingID, ok := db.Names[name]; ok && (snapshotID == "" || existingID == snapshotID) {
		delete(db.Names, name)
		delete(db.Snapshots, existingID)
		return p.writeSnapshotDB(db)
	}
	return nil
}

func (p *EpochPuller) cloudImageExists(ctx context.Context, name string) bool {
	imageDB := filepath.Join(p.rootDir, "cloudimg", "db", "images.json")
	data, err := os.ReadFile(imageDB)
	if err == nil {
		var db cloudImageIndex
		if jsonErr := json.Unmarshal(data, &db); jsonErr == nil {
			if entry := db.Images[name]; entry != nil {
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

func (p *EpochPuller) cocoonExec(ctx context.Context, args ...string) (string, error) {
	sudoArgs := []string{}
	root := strings.TrimSpace(os.Getenv("COCOON_ROOT_DIR"))
	if root == "" {
		root = strings.TrimSpace(p.rootDir)
	}
	if root != "" {
		sudoArgs = append(sudoArgs, "env", "COCOON_ROOT_DIR="+root)
	}
	if windowsCH := strings.TrimSpace(os.Getenv("COCOON_WINDOWS_CH_BINARY")); windowsCH != "" {
		if len(sudoArgs) == 0 {
			sudoArgs = append(sudoArgs, "env")
		}
		sudoArgs = append(sudoArgs, "COCOON_WINDOWS_CH_BINARY="+windowsCH)
	}
	sudoArgs = append(sudoArgs, p.cocoonBin)
	sudoArgs = append(sudoArgs, args...)
	cmd := exec.CommandContext(ctx, "sudo", sudoArgs...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	return string(out), err
}
