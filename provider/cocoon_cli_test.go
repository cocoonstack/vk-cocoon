package provider

import (
	"os"
	"path/filepath"
	"testing"

	epochcocoon "github.com/cocoonstack/epoch/cocoon"
)

func TestResolveCloneBootImageUsesSnapshotDiskFromCocoonDB(t *testing.T) {
	root := t.TempDir()
	t.Setenv("COCOON_ROOT_DIR", root)

	paths := epochcocoon.NewPaths(root)
	if err := paths.WriteSnapshotDB(&epochcocoon.SnapshotDB{
		Snapshots: map[string]*epochcocoon.SnapshotRecord{
			"sid-1": {
				ID:      "sid-1",
				Name:    "snap-1",
				DataDir: paths.SnapshotDataDir("sid-1"),
			},
		},
		Names: map[string]string{"snap-1": "sid-1"},
	}); err != nil {
		t.Fatalf("write snapshot db: %v", err)
	}

	want := filepath.Join(paths.SnapshotDataDir("sid-1"), "overlay.qcow2")
	if err := os.MkdirAll(paths.SnapshotDataDir("sid-1"), 0o755); err != nil {
		t.Fatalf("mkdir snapshot dir: %v", err)
	}
	if err := os.WriteFile(want, []byte("qcow2"), 0o644); err != nil {
		t.Fatalf("write snapshot disk: %v", err)
	}

	if got := resolveCloneBootImage("snap-1"); got != want {
		t.Fatalf("resolveCloneBootImage = %q, want %q", got, want)
	}
}

func TestResolveCloneBootImageFallsBackToOriginalRef(t *testing.T) {
	if got := resolveCloneBootImage("missing-snapshot"); got != "missing-snapshot" {
		t.Fatalf("resolveCloneBootImage = %q, want missing-snapshot", got)
	}
}
