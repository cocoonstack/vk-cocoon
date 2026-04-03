package provider

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocoonstack/epoch/manifest"
	"github.com/projecteru2/core/log"
)

// snapshotExportEnvelope matches cocoon's types.SnapshotExport.
type snapshotExportEnvelope struct {
	Version int                  `json:"version"`
	Config  snapshotExportConfig `json:"config"`
}

type snapshotExportConfig struct {
	ID           string              `json:"id,omitempty"`
	Name         string              `json:"name"`
	Image        string              `json:"image,omitempty"`
	ImageBlobIDs map[string]struct{} `json:"image_blob_ids,omitempty"`
	CPU          int                 `json:"cpu,omitempty"`
	Memory       int64               `json:"memory,omitempty"`
	Storage      int64               `json:"storage,omitempty"`
	NICs         int                 `json:"nics,omitempty"`
}

const snapshotJSONName = "snapshot.json"

// importSnapshotViaCLI builds a tar.gz archive from downloaded blobs and
// passes it to `cocoon snapshot import`, letting cocoon handle DB
// registration under flock.
func (p *EpochPuller) importSnapshotViaCLI(ctx context.Context, name, dataDir string, m *manifest.Manifest) error {
	logger := log.WithFunc("provider.importSnapshotViaCLI")

	archivePath := dataDir + ".tar.gz"
	if err := buildSnapshotArchive(archivePath, name, dataDir, m); err != nil {
		return fmt.Errorf("build snapshot archive: %w", err)
	}
	defer os.Remove(archivePath) //nolint:errcheck

	out, err := p.cocoonExec(ctx, "snapshot", "import", archivePath, "--name", name)
	if err != nil {
		return fmt.Errorf("cocoon snapshot import: %s: %w", strings.TrimSpace(out), err)
	}

	// Clean up the temp download directory — cocoon import copied the data.
	_ = os.RemoveAll(dataDir)

	logger.Infof(ctx, "imported snapshot %s via cocoon CLI", name)
	return nil
}

// buildSnapshotArchive creates a gzip-compressed tar at archivePath containing
// snapshot.json metadata followed by all files in dataDir.
func buildSnapshotArchive(archivePath, name, dataDir string, m *manifest.Manifest) (retErr error) {
	f, err := os.Create(archivePath) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && retErr == nil {
			retErr = closeErr
		}
	}()

	gw, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	tw := tar.NewWriter(gw)

	// Write snapshot.json metadata.
	envelope := snapshotExportEnvelope{
		Version: 1,
		Config: snapshotExportConfig{
			ID:      m.SnapshotID,
			Name:    name,
			Image:   m.Image,
			CPU:     m.CPU,
			Memory:  m.Memory,
			Storage: m.Storage,
			NICs:    m.NICs,
		},
	}
	jsonData, jsonErr := json.MarshalIndent(envelope, "", "  ")
	if jsonErr != nil {
		return fmt.Errorf("marshal snapshot.json: %w", jsonErr)
	}
	jsonData = append(jsonData, '\n')

	if writeErr := tw.WriteHeader(&tar.Header{
		Name: snapshotJSONName, Size: int64(len(jsonData)),
		Mode: 0o644, ModTime: time.Now(),
	}); writeErr != nil {
		return writeErr
	}
	if _, writeErr := tw.Write(jsonData); writeErr != nil {
		return writeErr
	}

	// Write data files.
	entries, readErr := os.ReadDir(dataDir)
	if readErr != nil {
		return fmt.Errorf("read data dir: %w", readErr)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if tarErr := tarFile(tw, filepath.Join(dataDir, entry.Name())); tarErr != nil {
			return tarErr
		}
	}

	if closeErr := tw.Close(); closeErr != nil {
		return closeErr
	}
	return gw.Close()
}

func tarFile(tw *tar.Writer, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if hdrErr := tw.WriteHeader(&tar.Header{
		Name: filepath.Base(path), Size: info.Size(),
		Mode: 0o644, ModTime: info.ModTime(),
	}); hdrErr != nil {
		return hdrErr
	}
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	_, err = io.Copy(tw, f)
	return err
}

// exportSnapshotForPush calls `cocoon snapshot export <name>` to produce
// a tar.gz archive, then extracts it to a temp directory for blob upload.
func (p *EpochPuller) exportSnapshotForPush(ctx context.Context, name string) (_ *snapshotExportConfig, _ string, retErr error) {
	logger := log.WithFunc("provider.exportSnapshotForPush")

	archivePath := filepath.Join(os.TempDir(), fmt.Sprintf("epoch-export-%s-%d.tar.gz", name, time.Now().UnixNano()))
	out, err := p.cocoonExec(ctx, "snapshot", "export", name, "--output", archivePath)
	if err != nil {
		return nil, "", fmt.Errorf("cocoon snapshot export: %s: %w", strings.TrimSpace(out), err)
	}
	defer os.Remove(archivePath) //nolint:errcheck

	tmpDir, mkErr := os.MkdirTemp("", "epoch-push-*")
	if mkErr != nil {
		return nil, "", fmt.Errorf("create temp dir: %w", mkErr)
	}
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	af, openErr := os.Open(archivePath) //nolint:gosec
	if openErr != nil {
		return nil, "", openErr
	}
	defer af.Close() //nolint:errcheck

	cfg, extractErr := extractSnapshotArchive(af, tmpDir)
	if extractErr != nil {
		return nil, "", fmt.Errorf("extract snapshot archive: %w", extractErr)
	}

	logger.Infof(ctx, "exported snapshot %s to %s", name, tmpDir)
	return cfg, tmpDir, nil
}

// extractSnapshotArchive reads a gzip tar stream, extracts files to destDir,
// and returns the snapshot config from snapshot.json.
func extractSnapshotArchive(r io.Reader, destDir string) (*snapshotExportConfig, error) {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	defer gr.Close() //nolint:errcheck

	tr := tar.NewReader(gr)
	var cfg *snapshotExportConfig

	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, fmt.Errorf("read tar header: %w", nextErr)
		}

		if hdr.Name == snapshotJSONName {
			var envelope snapshotExportEnvelope
			if decErr := json.NewDecoder(tr).Decode(&envelope); decErr != nil {
				return nil, fmt.Errorf("parse snapshot.json: %w", decErr)
			}
			cfg = &envelope.Config
			continue
		}

		destPath := filepath.Join(destDir, filepath.Base(hdr.Name))
		if writeErr := extractTarEntry(destPath, tr, hdr.Size); writeErr != nil {
			return nil, fmt.Errorf("write %s: %w", hdr.Name, writeErr)
		}
	}

	if cfg == nil {
		return nil, fmt.Errorf("snapshot.json not found in archive")
	}
	return cfg, nil
}

func extractTarEntry(destPath string, r io.Reader, size int64) error {
	f, err := os.Create(destPath) //nolint:gosec
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	_, err = io.Copy(f, io.LimitReader(r, size)) //nolint:gosec // size from trusted tar header
	return err
}
