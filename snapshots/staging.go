package snapshots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon-common/manifest"
)

// newStagingDir creates a fresh dir under root for one restore. root MUST
// live on the same filesystem as cocoon's data dir: clone hardlinks memory
// files, and the cross-device fallback is a symlink that deleting the staging
// dir would break.
func newStagingDir(root, localName string) (string, error) {
	if root == "" {
		return "", errors.New("staging root is not configured")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", fmt.Errorf("create staging root: %w", err)
	}
	dir, err := os.MkdirTemp(root, sanitizeStagingPrefix(localName)+"-")
	if err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}
	return dir, nil
}

// SweepStaging removes every entry under root — leftovers are crashed
// restores, and staged data is re-pullable by construction. Call once at
// startup, before the provider serves.
func SweepStaging(ctx context.Context, root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.WithFunc("snapshots.SweepStaging").Errorf(ctx, err, "read staging root %s", root)
		}
		return
	}
	logger := log.WithFunc("snapshots.SweepStaging")
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if err := os.RemoveAll(p); err != nil {
			logger.Errorf(ctx, err, "remove stale staging entry %s", p)
			continue
		}
		logger.Infof(ctx, "removed stale staging entry %s", p)
	}
}

func sanitizeStagingPrefix(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		default:
			return '_'
		}
	}, name)
}

// snapshotEnvelope mirrors cocoon's snapshot.json (types.SnapshotExport):
// what `vm clone --from-dir` reads next to the data files.
type snapshotEnvelope struct {
	Version int                    `json:"version"`
	Config  snapshotEnvelopeConfig `json:"config"`
}

type snapshotEnvelopeConfig struct {
	CPU          int                 `json:"cpu,omitempty"`
	Memory       int64               `json:"memory,omitempty"`
	Storage      int64               `json:"storage,omitempty"`
	Image        string              `json:"image,omitempty"`
	ImageDigest  string              `json:"image_digest,omitempty"`
	ImageType    string              `json:"image_type,omitempty"`
	Network      string              `json:"network,omitempty"`
	Windows      bool                `json:"windows,omitempty"`
	ID           string              `json:"id,omitempty"`
	Name         string              `json:"name"`
	Description  string              `json:"description,omitempty"`
	ImageBlobIDs map[string]struct{} `json:"image_blob_ids,omitempty"`
	Hypervisor   string              `json:"hypervisor,omitempty"`
	NICs         int                 `json:"nics,omitempty"`
}

// writeEnvelope synthesizes snapshot.json from the registry config blob — the
// registry stays the trust anchor for snapshot identity even when the bytes
// came from a peer.
func writeEnvelope(dir string, cfg *manifest.SnapshotConfig, localName string) error {
	envelope := snapshotEnvelope{
		Version: 1,
		Config: snapshotEnvelopeConfig{
			CPU:          cfg.CPU,
			Memory:       cfg.Memory,
			Storage:      cfg.Storage,
			Image:        cfg.Image,
			ImageDigest:  cfg.ImageDigest,
			ImageType:    cfg.ImageType,
			Network:      cfg.Network,
			Windows:      cfg.Windows,
			ID:           cfg.SnapshotID,
			Name:         localName,
			Description:  cfg.Description,
			ImageBlobIDs: cfg.ImageBlobIDs,
			Hypervisor:   cfg.Hypervisor,
			NICs:         cfg.NICs,
		},
	}
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot envelope: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "snapshot.json"), append(data, '\n'), 0o644) //nolint:gosec // envelope is 0644, matching cocoon's export
}
