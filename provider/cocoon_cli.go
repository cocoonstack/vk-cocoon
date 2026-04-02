package provider

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"

	epochcocoon "github.com/cocoonstack/epoch/cocoon"
)

func buildDeleteArgs(ref string) []string {
	return []string{"delete", "--force", ref}
}

func buildInspectArgs(ref string) []string {
	return []string{"inspect", ref}
}

func buildListArgs() []string {
	return []string{"list", "--all", "--format", "json"}
}

func cocoonRootDir() string {
	return cmp.Or(strings.TrimSpace(os.Getenv("COCOON_ROOT_DIR")), epochcocoon.DefaultRootDir)
}

func resolveCloneBootImage(snapshot string) string {
	snapshot = strings.TrimSpace(snapshot)
	if snapshot == "" {
		return snapshot
	}
	if fi, err := os.Stat(snapshot); err == nil && !fi.IsDir() {
		return snapshot
	}

	paths := epochcocoon.NewPaths(cocoonRootDir())
	snapshotID, err := paths.ResolveSnapshotID(snapshot)
	if err != nil {
		return snapshot
	}
	if disk := snapshotDiskPath(paths.SnapshotDataDir(snapshotID)); disk != "" {
		return disk
	}
	return snapshot
}

func snapshotDiskPath(dataDir string) string {
	for _, preferred := range []string{"overlay.qcow2", "cow.raw"} {
		path := filepath.Join(dataDir, preferred)
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			return path
		}
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return ""
	}

	var qcow2 []string
	var raw []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch {
		case strings.HasSuffix(name, ".qcow2"):
			qcow2 = append(qcow2, filepath.Join(dataDir, name))
		case strings.HasSuffix(name, ".raw"):
			raw = append(raw, filepath.Join(dataDir, name))
		}
	}
	slices.Sort(qcow2)
	slices.Sort(raw)
	if len(qcow2) > 0 {
		return qcow2[0]
	}
	if len(raw) > 0 {
		return raw[0]
	}
	return ""
}
