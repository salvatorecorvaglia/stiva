package storage

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewBlobRejectsTraversal(t *testing.T) {
	engine := setupTestEngine(t)
	for _, bucket := range []string{"../outside", "safe/../../outside", "/outside", `..\outside`} {
		if _, _, err := engine.newBlob(bucket); err == nil {
			t.Errorf("newBlob(%q) accepted invalid bucket", bucket)
		}
	}
}

func TestNewBlobRejectsEscapingSymlink(t *testing.T) {
	engine := setupTestEngine(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(engine.dataDir, "blobs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, engine.blobDir("safe-bucket")); err != nil {
		t.Skip(err)
	}
	if _, _, err := engine.newBlob("safe-bucket"); err == nil {
		t.Fatal("created blob directory through escaping symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("created directories outside storage root")
	}
}

func TestSyncParentsConfinedToRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if err := syncParents(nested, root); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := syncParents(outside, root); err == nil {
		t.Fatal("accepted directory outside root")
	}
	if runtime.GOOS == "windows" {
		t.Skip("directory syncing is a no-op on Windows")
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip(err)
	}
	if err := syncParents(link, root); err == nil {
		t.Fatal("synced through escaping symlink")
	}
}
