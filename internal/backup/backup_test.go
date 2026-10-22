package backup_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/backup"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

func TestBackupRestoreAndCorruption(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	archive := filepath.Join(root, "archive")
	restored := filepath.Join(root, "restored")
	engine, err := storage.NewFilesystemEngine(data, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.CreateBucket("production"); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("backup payload", 1000)
	if _, err := engine.PutObject(context.Background(), "production", "nested/data.db", strings.NewReader(payload), int64(len(payload)), "text/plain"); err != nil {
		t.Fatal(err)
	}
	// A live server owns the central database exclusively.
	if err := backup.Create(data, archive); err == nil {
		t.Fatal("backup accepted a running engine")
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := backup.Create(data, archive); err != nil {
		t.Fatal(err)
	}
	if err := backup.Verify(archive); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(archive, restored); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(archive, restored); err == nil {
		t.Fatal("restore overwrote an existing destination")
	}
	recovered, err := storage.NewFilesystemEngine(restored, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	r, _, err := recovered.GetObject(context.Background(), "production", "nested/data.db", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(got) != payload {
		t.Fatalf("restored data mismatch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archive, "extra"), []byte("unexpected"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := backup.Verify(archive); err == nil {
		t.Fatal("extra file accepted")
	}
	if err := os.Remove(filepath.Join(archive, "extra")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archive, "stiva.db"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(archive, filepath.Join(root, "bad")); err == nil {
		t.Fatal("corrupt backup restored")
	}
}

func TestBackupRejectsNestedDestination(t *testing.T) {
	root := t.TempDir()
	if err := backup.Create(root, filepath.Join(root, "nested")); err == nil {
		t.Fatal("nested backup accepted")
	}
}

func TestBackupRejectsMalformedAndUnsafeSources(t *testing.T) {
	root := t.TempDir()
	for _, raw := range []string{"{", `{"version":2,"files":{"stiva.db":"x"}}`, `{"version":1,"files":{}}`} {
		p := filepath.Join(root, "backup-manifest.json")
		if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := backup.Verify(root); err == nil {
			t.Fatalf("invalid manifest accepted: %s", raw)
		}
	}
	if err := backup.Verify(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing backup accepted")
	}
	if err := backup.Restore(root, filepath.Join(root, "restore")); err == nil {
		t.Fatal("invalid backup restored")
	}
	// A symlink anywhere in a backup is rejected before reading its target.
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := backup.Verify(root); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := backup.Create(root, filepath.Join(t.TempDir(), "backup")); err == nil {
		t.Fatal("symlink data directory accepted")
	}
}
