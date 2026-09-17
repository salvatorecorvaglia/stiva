package storage

import (
	"path/filepath"
	"strings"
	"testing"
)

// FuzzObjectPathContainment asserts the core security invariant of the storage
// layer: whatever key an attacker supplies, objectPath either rejects it or
// returns a path inside the bucket directory. Path validation had no fuzzing
// despite being the only thing standing between a crafted key and the host
// filesystem.
func FuzzObjectPathContainment(f *testing.F) {
	seeds := []string{
		"normal.txt",
		"nested/dir/file.txt",
		"../escape.txt",
		"../../etc/passwd",
		"a/../../b",
		"..",
		"./x",
		"/absolute.txt",
		"\\windows\\style",
		"..\\..\\escape",
		"a/./b/../c",
		"....//....//etc",
		"%2e%2e/escape",
		"a\x00b",
		strings.Repeat("../", 40) + "etc/passwd",
		strings.Repeat("a/", 200) + "deep.txt",
		"unicode-ü/ok.txt",
		"trailing/",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	fs := newFilesystemEngineForTesting("/srv/stiva-data")
	base := filepath.Clean(fs.bucketPath("bucket"))
	basePrefix := base + string(filepath.Separator)

	f.Fuzz(func(t *testing.T, key string) {
		got, err := fs.objectPath("bucket", key)
		if err != nil {
			return // Rejection is always an acceptable outcome.
		}

		// If it was accepted, the result must stay inside the bucket.
		clean := filepath.Clean(got)
		if clean != base && !strings.HasPrefix(clean, basePrefix) {
			t.Fatalf("key %q escaped the bucket directory: %q", key, got)
		}

		rel, relErr := filepath.Rel(base, clean)
		if relErr != nil {
			t.Fatalf("key %q produced a path with no relation to the base: %q", key, got)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("key %q resolved outside the bucket: rel=%q", key, rel)
		}
		if filepath.IsAbs(rel) {
			t.Fatalf("key %q produced an absolute relative path: %q", key, rel)
		}
	})
}

// FuzzMultipartDirContainment applies the same invariant to multipart part
// directories, which combine an attacker-influenced key and upload ID.
func FuzzMultipartDirContainment(f *testing.F) {
	for _, seed := range []struct{ key, uploadID string }{
		{"file.bin", "abc-123"},
		{"../escape", "abc-123"},
		{"file.bin", "../escape"},
		{"a/b/c", "../../.."},
		{"..", ".."},
		{"", ""},
		{"ok", "a/b"},
	} {
		f.Add(seed.key, seed.uploadID)
	}

	fs := newFilesystemEngineForTesting("/srv/stiva-data")

	f.Fuzz(func(t *testing.T, key, uploadID string) {
		got, err := fs.multipartDir("bucket", key, uploadID)
		if err != nil {
			return
		}

		base := filepath.Clean(filepath.Join(fs.DataDir(), "multipart", "bucket"))
		basePrefix := base + string(filepath.Separator)
		clean := filepath.Clean(got)

		if clean != base && !strings.HasPrefix(clean, basePrefix) {
			t.Fatalf("key=%q uploadID=%q escaped the multipart directory: %q", key, uploadID, got)
		}
	})
}
