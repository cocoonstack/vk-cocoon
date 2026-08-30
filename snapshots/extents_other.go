//go:build !linux

package snapshots

import "os"

// scanExtents without SEEK_DATA support: one dense extent; holes transfer as zeros.
func scanExtents(f *os.File) (int64, []extent, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, nil, err
	}
	if fi.Size() == 0 {
		return 0, nil, nil
	}
	return fi.Size(), []extent{{offset: 0, length: fi.Size()}}, nil
}
