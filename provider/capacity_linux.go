package provider

import "syscall"

type syscallStatfs = syscall.Statfs_t

func statfs(path string, buf *syscallStatfs) error {
	return syscall.Statfs(path, buf)
}

// statTotalBytes returns the total filesystem size. Bsize is int64 on linux.
func statTotalBytes(stat syscallStatfs) int64 {
	return int64(stat.Blocks) * stat.Bsize //nolint:gosec // block count fits int64 on any real filesystem
}

func statAvailBytes(stat syscallStatfs) int64 {
	return int64(stat.Bavail) * stat.Bsize //nolint:gosec // block count fits int64 on any real filesystem
}
