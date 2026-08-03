//go:build !linux

package snapshots

import "os"

type extent struct {
	offset int64
	length int64
}

// scanExtents without SEEK_DATA support: one dense extent. Holes transfer as
// zeros; correctness is identical, only wire bytes differ.
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
