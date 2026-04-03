package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/epoch/cocoon"
	"github.com/cocoonstack/epoch/manifest"
)

// EnsureSnapshot ensures a snapshot is available locally, pulling via HTTP if needed.
func (p *EpochPuller) EnsureSnapshot(ctx context.Context, name string) error {
	return p.EnsureSnapshotTag(ctx, name, "latest")
}

// EnsureSnapshotTag ensures a specific tag is available locally.
func (p *EpochPuller) EnsureSnapshotTag(ctx context.Context, name, tag string) error {
	if p.ensureSnapshotTagFn != nil {
		return p.ensureSnapshotTagFn(ctx, name, tag)
	}

	ref := name + ":" + tag
	if p.cachedPull(ref) {
		return nil
	}

	if p.localSnapshotExists(ctx, name) {
		p.markPulled(ref)
		return nil
	}

	log.WithFunc("provider.EnsureSnapshotTag").Infof(ctx, "[epoch] pulling %s via HTTP...", ref)
	start := time.Now()

	if err := p.pull(ctx, name, tag); err != nil {
		return fmt.Errorf("epoch HTTP pull %s: %w", ref, err)
	}

	log.WithFunc("provider.EnsureSnapshotTag").Infof(ctx, "[epoch] %s pulled in %s", ref, time.Since(start).Round(time.Second))
	p.markPulled(ref)
	return nil
}

func (p *EpochPuller) pull(ctx context.Context, name, tag string) error {
	m, err := p.getManifest(ctx, name, tag)
	if err != nil {
		return fmt.Errorf("get manifest: %w", err)
	}
	if isCloudImageManifest(m) {
		return fmt.Errorf("manifest %s:%s is a cloud image, not a snapshot", name, tag)
	}

	sid := m.SnapshotID
	dataDir := p.paths.SnapshotDataDir(sid)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dataDir, err)
	}

	for _, layer := range m.Layers {
		destPath := filepath.Join(dataDir, layer.Filename)
		if _, err := os.Stat(destPath); err == nil {
			log.WithFunc("provider.pull").Infof(ctx, "[epoch]   %s exists, skip", layer.Filename)
			continue
		}
		log.WithFunc("provider.pull").Infof(ctx, "[epoch]   downloading %s (%s)...", layer.Filename, cocoon.HumanSize(layer.Size))
		if err := p.downloadBlob(ctx, name, layer.Digest, destPath); err != nil {
			return fmt.Errorf("download %s: %w", layer.Filename, err)
		}
	}

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

	return p.importSnapshotViaCLI(ctx, name, dataDir, m)
}

func (p *EpochPuller) getManifest(ctx context.Context, name, tag string) (*manifest.Manifest, error) {
	url := p.manifestURL(name, tag)
	if p.authToken() != "" {
		return p.getManifestWithCurl(ctx, url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	p.setAuth(req)

	resp, err := p.client.Do(req) //nolint:gosec // epoch serverURL is configured by the trusted provider setup
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get manifest %s:%s: %d %s", name, tag, resp.StatusCode, readLimitedBody(resp.Body))
	}

	return decodeJSON[manifest.Manifest](resp.Body)
}

func (p *EpochPuller) downloadBlob(ctx context.Context, name, digest, destPath string) error {
	url := p.blobURL(name, digest)
	if p.authToken() != "" {
		return p.downloadBlobWithCurl(ctx, url, destPath)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	p.setAuth(req)

	resp, err := p.client.Do(req) //nolint:gosec // epoch serverURL is configured by the trusted provider setup
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get blob %s: %d %s", digest[:12], resp.StatusCode, readLimitedBody(resp.Body))
	}

	f, err := os.Create(destPath) //nolint:gosec // destPath is constructed from trusted config
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	_, err = io.Copy(f, resp.Body)
	return err
}

// Epoch's public gateway intermittently rejects token-authenticated requests
// from Go's client stack while equivalent curl requests succeed on the same host.
// When a registry token is configured, use curl for the whole pull path so
// manifest/blob requests stay on the same known-good client behavior.
func (p *EpochPuller) getManifestWithCurl(ctx context.Context, url string) (*manifest.Manifest, error) {
	data, err := p.curlRead(ctx, url)
	if err != nil {
		return nil, err
	}
	return decodeJSON[manifest.Manifest](strings.NewReader(string(data)))
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
	var ee *exec.ExitError
	if errors.As(err, &ee) {
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

func readLimitedBody(body io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(body, 512))
	return string(data)
}

func decodeJSON[T any](reader io.Reader) (*T, error) {
	var out T
	if err := json.NewDecoder(reader).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
