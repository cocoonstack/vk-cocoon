//go:build linux

package vm

import (
	"os"

	"golang.org/x/sys/unix"
)

const transferPipeBytes = 8 << 20

func widenPipe(p any) {
	f, ok := p.(*os.File)
	if !ok {
		return
	}
	_, _ = unix.FcntlInt(f.Fd(), unix.F_SETPIPE_SZ, transferPipeBytes)
}
