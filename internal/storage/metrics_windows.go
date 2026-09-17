//go:build windows

package storage

import (
	"golang.org/x/sys/windows"
)

// GetDiskSpace retrieves total and free space of the filesystem containing path on Windows.
func GetDiskSpace(path string) (total uint64, free uint64, err error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeBytes, totalBytes, totalFreeBytes uint64
	err = windows.GetDiskFreeSpaceEx(pathPtr, &freeBytes, &totalBytes, &totalFreeBytes)
	if err != nil {
		return 0, 0, err
	}
	return totalBytes, freeBytes, nil
}
