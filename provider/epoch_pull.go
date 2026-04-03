package provider

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/cocoonstack/epoch/cocoon"
	"github.com/cocoonstack/epoch/manifest"
	"github.com/projecteru2/core/log"
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

// pull streams a snapshot from the registry directly into cocoon via pipe.
// Flow: GetManifest → stream gzip tar (snapshot.json + blobs) → cocoon snapshot import stdin.
func (p *EpochPuller) pull(ctx context.Context, name, tag string) error {
	logger := log.WithFunc("provider.pull")

	m, err := p.getManifest(ctx, name, tag)
	if err != nil {
		return fmt.Errorf("get manifest: %w", err)
	}
	if m.IsCloudImage() {
		return fmt.Errorf("manifest %s:%s is a cloud image, not a snapshot", name, tag)
	}

	// Download base images first (still file-based, shared blobs).
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

	// Stream snapshot tar.gz into cocoon snapshot import via pipe.
	if err := p.pipeToImport(ctx, []string{"snapshot", "import", "--name", name}, func(w io.Writer) error {
		return p.writeSnapshotStream(ctx, name, m, w)
	}); err != nil {
		return err
	}

	logger.Infof(ctx, "[epoch] snapshot %s:%s imported via stream", name, tag)
	return nil
}

// writeSnapshotStream writes a gzip-compressed tar archive to w,
// streaming each blob directly from the registry HTTP response.
func (p *EpochPuller) writeSnapshotStream(ctx context.Context, name string, m *manifest.Manifest, w io.Writer) error {
	logger := log.WithFunc("provider.writeSnapshotStream")

	blobIDs := make(map[string]struct{}, len(m.ImageBlobIDs))
	for k := range m.ImageBlobIDs {
		blobIDs[k] = struct{}{}
	}

	envelope := snapshotExportEnvelope{
		Version: 1,
		Config: snapshotExportConfig{
			ID:           m.SnapshotID,
			Name:         name,
			Image:        m.Image,
			ImageBlobIDs: blobIDs,
			CPU:          m.CPU,
			Memory:       m.Memory,
			Storage:      m.Storage,
			NICs:         m.NICs,
		},
	}
	jsonData, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot metadata: %w", err)
	}
	jsonData = append(jsonData, '\n')

	now := time.Now()
	bw := bufio.NewWriterSize(w, 256<<10)
	gw, _ := gzip.NewWriterLevel(bw, gzip.BestSpeed)
	tw := tar.NewWriter(gw)

	if err := tw.WriteHeader(&tar.Header{
		Name: snapshotJSONName, Size: int64(len(jsonData)),
		Mode: 0o644, ModTime: now,
	}); err != nil {
		return fmt.Errorf("write snapshot.json header: %w", err)
	}
	if _, err := tw.Write(jsonData); err != nil {
		return fmt.Errorf("write snapshot.json: %w", err)
	}

	for _, layer := range m.Layers {
		logger.Infof(ctx, "[epoch]   streaming %s (%s)...", layer.Filename, cocoon.HumanSize(layer.Size))

		if err := p.streamBlobToTar(ctx, name, layer, tw, now); err != nil {
			return fmt.Errorf("stream %s: %w", layer.Filename, err)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}
	return bw.Flush()
}

// streamBlobToTar downloads a blob from the registry and writes it as a tar entry.
func (p *EpochPuller) streamBlobToTar(ctx context.Context, name string, layer manifest.Layer, tw *tar.Writer, modTime time.Time) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: layer.Filename, Size: layer.Size,
		Mode: 0o640, ModTime: modTime,
	}); err != nil {
		return fmt.Errorf("write tar header: %w", err)
	}

	body, err := p.streamBlob(ctx, name, layer.Digest)
	if err != nil {
		return fmt.Errorf("get blob: %w", err)
	}
	defer body.Close() //nolint:errcheck

	if _, err := io.Copy(tw, body); err != nil {
		return fmt.Errorf("copy blob data: %w", err)
	}
	return nil
}

// streamBlob returns a streaming reader for a blob from the registry.
func (p *EpochPuller) streamBlob(ctx context.Context, name, digest string) (io.ReadCloser, error) {
	url := p.blobURL(name, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	p.setAuth(req)

	resp, err := p.client.Do(req) //nolint:gosec // epoch serverURL is configured by the trusted provider setup
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, fmt.Errorf("get blob %s: %d %s", digest[:12], resp.StatusCode, readLimitedBody(resp.Body))
	}
	return resp.Body, nil
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

func (p *EpochPuller) getManifestWithCurl(ctx context.Context, url string) (*manifest.Manifest, error) {
	data, err := p.curlRead(ctx, url)
	if err != nil {
		return nil, err
	}
	return decodeJSON[manifest.Manifest](bytes.NewReader(data))
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
