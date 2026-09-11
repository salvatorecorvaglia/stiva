package storage_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// TestStartupSweepRemovesOrphanedTempFiles covers the startup sweep that
// reclaims partial files left by a crash. Its prefix list omitted
// "stiva-chunked-" — the temp files decodeStreamingPayload writes for
// aws-chunked uploads — so those were never reclaimed, by the sweep or
// otherwise.
func TestStartupSweepRemovesOrphanedTempFiles(t *testing.T) {
	dataDir := t.TempDir()

	dirs := map[string]string{
		"buckets":   filepath.Join(dataDir, "buckets"),
		"multipart": filepath.Join(dataDir, "multipart"),
		"tmp":       filepath.Join(dataDir, "tmp"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	orphans := []string{
		filepath.Join(dirs["buckets"], ".stiva-tmp-123"),
		filepath.Join(dirs["buckets"], ".stiva-multipart-123"),
		filepath.Join(dirs["multipart"], ".part-tmp-123"),
		filepath.Join(dirs["tmp"], "stiva-body-123"),
		filepath.Join(dirs["tmp"], "stiva-chunked-123"),
	}
	for _, f := range orphans {
		if err := os.WriteFile(f, []byte("partial"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}

	// A real object file must survive the sweep.
	keep := filepath.Join(dirs["buckets"], "real-object.txt")
	if err := os.WriteFile(keep, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write %s: %v", keep, err)
	}

	eng, err := storage.NewFilesystemEngine(dataDir, nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	defer eng.Close()

	for _, f := range orphans {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("orphaned temp file %s was not swept", filepath.Base(f))
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("sweep removed a real object file: %v", err)
	}
}
