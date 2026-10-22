//go:build windows

package storage

// Windows does not support directory File.Sync through Go. File contents are
// still flushed; the production durability guarantee targets Linux filesystems.
func syncDirectory(_ string) error { return nil }
