package snapshots

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/projecteru2/core/log"

	"github.com/cocoonstack/cocoon-common/manifest"
	"github.com/cocoonstack/cocoon-common/snapshot"
)

// newStagingDir creates a fresh dir under root. root MUST share a filesystem
// with cocoon's data dir: clone hardlinks memory files, and the cross-device
// fallback is a symlink the staging delete would break.
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

// SweepStaging removes every entry under root at startup — leftovers are
// crashed restores, re-pullable by construction.
func SweepStaging(ctx context.Context, root string) {
	logger := log.WithFunc("snapshots.SweepStaging")
	entries, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Errorf(ctx, err, "read staging root %s", root)
		}
		return
	}
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

// writeEnvelope synthesizes snapshot.json from the registry config blob, so
// the registry stays the identity anchor for peer-fetched bytes.
func writeEnvelope(dir string, cfg *manifest.SnapshotConfig, localName string) error {
	data, err := snapshot.MarshalEnvelope(cfg, localName)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "snapshot.json"), data, 0o644) //nolint:gosec // envelope is 0644, matching cocoon's export
}
