//go:build linux

package snapshots

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// scanExtents enumerates data runs via SEEK_DATA/SEEK_HOLE so holes in sparse
// snapshot files (memory ranges, overlays) never cross the wire. Filesystems
// without hole support degrade to one dense extent.
func scanExtents(f *os.File) (int64, []extent, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, nil, err
	}

	size := fi.Size()
	if size == 0 {
		return 0, nil, nil
	}

	var (
		extents []extent
		fd      = int(f.Fd())
	)

	for off := int64(0); off < size; {
		dataStart, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break // only holes remain
			}
			if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
				return size, []extent{{offset: 0, length: size}}, nil
			}
			return 0, nil, fmt.Errorf("seek data at %d: %w", off, err)
		}

		holeStart, err := unix.Seek(fd, dataStart, unix.SEEK_HOLE)
		if err != nil {
			return 0, nil, fmt.Errorf("seek hole at %d: %w", dataStart, err)
		}

		extents = append(extents, extent{offset: dataStart, length: holeStart - dataStart})
		off = holeStart
	}
	return size, extents, nil
}
