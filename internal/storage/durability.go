package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// syncParents persists the directory entries from path through root. Newly
// created blob shards need their parents synced too, not just the leaf.
func syncParents(path, root string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("directory outside data root")
	}
	dirRoot, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer dirRoot.Close()
	for {
		if err := syncDirectory(dirRoot, rel); err != nil {
			return fmt.Errorf("sync directory %s: %w", path, err)
		}
		if path == root {
			return nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return fmt.Errorf("directory outside data root")
		}
		path = parent
		rel = filepath.Dir(rel)
	}
}
