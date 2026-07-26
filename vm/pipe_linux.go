//go:build linux

package vm

import (
	"os"

	"golang.org/x/sys/unix"
)

// transferPipeBytes sizes the pipes carrying snapshot and image bytes to and from
// cocoon; see widenPipe for why the 64 KiB default caps them.
const transferPipeBytes = 8 << 20

// widenPipe grows the kernel pipe buffer feeding cocoon import/export. At the
// 64 KiB default, vk and the subprocess ping-pong: neither side saturates CPU
// yet a snapshot pull stalls near 300 MiB/s because 64 KiB cannot absorb either
// side's bursts. Going past /proc/sys/fs/pipe-max-size needs CAP_SYS_RESOURCE,
// so a failure here is not worth reporting — the transfer still works.
func widenPipe(p any) {
	f, ok := p.(*os.File)
	if !ok {
		return
	}
	_, _ = unix.FcntlInt(f.Fd(), unix.F_SETPIPE_SZ, transferPipeBytes)
}
