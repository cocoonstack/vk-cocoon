//go:build linux

package vm

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWidenPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	limit, err := readPipeMaxSize()
	if err != nil {
		t.Fatalf("read pipe max size: %v", err)
	}
	if err := widenPipe(w); err != nil {
		t.Fatalf("widen pipe: %v", err)
	}
	got, err := unix.FcntlInt(r.Fd(), unix.F_GETPIPE_SZ, 0)
	if err != nil {
		t.Fatalf("get pipe size: %v", err)
	}
	if want := min(limit, transferPipeBytes); got < want {
		t.Errorf("pipe size = %d, want at least %d", got, want)
	}
}
