package vm

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCOWSizeCountsTheAllocatedBlocksOfASparseOverlay(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "run", runDirCH, "vm1")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "cow.raw"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{1}, 1<<20), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := COWSize(root, "cloud-hypervisor", "vm1"); got < 1<<20 || got >= 1<<29 {
		t.Errorf("COWSize = %d, want the ~1 MiB written, not the 1 GiB apparent size", got)
	}
	if got := COWSize(root, "cloud-hypervisor", "absent"); got != 0 {
		t.Errorf("COWSize of a VM without an overlay = %d, want 0", got)
	}
}
