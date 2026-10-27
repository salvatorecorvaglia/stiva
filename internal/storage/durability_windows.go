//go:build windows

package storage

import "os"

// Windows does not support directory File.Sync through Go. File contents are
// still flushed; the production durability guarantee targets Linux filesystems.
func syncDirectory(_ *os.Root, _ string) error { return nil }
