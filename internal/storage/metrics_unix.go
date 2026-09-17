//go:build !windows

package storage

import (
	"syscall"
)

// GetDiskSpace retrieves total and free space of the filesystem containing path on Unix-like systems.
func GetDiskSpace(path string) (total uint64, free uint64, err error) {
	var stat syscall.Statfs_t
	err = syscall.Statfs(path, &stat)
	if err != nil {
		return 0, 0, err
	}
	total = stat.Blocks * uint64(stat.Bsize) // #nosec G115
	free = stat.Bfree * uint64(stat.Bsize)   // #nosec G115
	return total, free, nil
}
