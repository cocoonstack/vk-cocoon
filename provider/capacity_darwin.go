package provider

import "syscall"

type syscallStatfs = syscall.Statfs_t

func statfs(path string, buf *syscallStatfs) error {
	return syscall.Statfs(path, buf)
}

// statTotalBytes returns the total filesystem size. Bsize is int32 on darwin.
func statTotalBytes(stat syscallStatfs) int64 {
	return int64(stat.Blocks) * int64(stat.Bsize) //nolint:gosec
}

func statAvailBytes(stat syscallStatfs) int64 {
	return int64(stat.Bavail) * int64(stat.Bsize) //nolint:gosec
}
