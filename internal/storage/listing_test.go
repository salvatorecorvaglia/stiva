package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func newListEngine(t *testing.T, bucket string, keys ...string) *FilesystemEngine {
	t.Helper()
	fs, err := NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	if err := fs.CreateBucket(bucket); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	for _, k := range keys {
		if _, err := fs.PutObject(context.Background(), bucket, k,
			strings.NewReader("x"), 1, "application/octet-stream"); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
	}
	return fs
}

// TestListObjectsPaginationNoDuplicates is the regression test for the audit
// finding that truncating on a CommonPrefix emitted a continuation token
// pointing at the last *object*, which sorts before the prefixes already
// returned. Paging then re-emitted those prefixes on every subsequent page.
func TestListObjectsPaginationNoDuplicates(t *testing.T) {
	fs := newListEngine(t, "testbucket",
		"aaa.txt", "d1/x", "d2/x", "d3/x", "d4/x")

	seenKeys := map[string]int{}
	seenPrefixes := map[string]int{}

	token := ""
	for page := 0; page < 10; page++ {
		out, err := fs.ListObjects(&ListObjectsInput{
			Bucket:            "testbucket",
			Delimiter:         "/",
			MaxKeys:           3,
			ContinuationToken: token,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, o := range out.Objects {
			seenKeys[o.Key]++
		}
		for _, p := range out.CommonPrefixes {
			seenPrefixes[p]++
		}
		if !out.IsTruncated {
			break
		}
		if out.NextContinuationToken == "" {
			t.Fatalf("page %d: truncated but no continuation token", page)
		}
		if out.NextContinuationToken == token {
			t.Fatalf("page %d: continuation token did not advance (%q)", page, token)
		}
		token = out.NextContinuationToken
	}

	for k, n := range seenKeys {
		if n != 1 {
			t.Errorf("object %q returned %d times across pages, want exactly 1", k, n)
		}
	}
	for p, n := range seenPrefixes {
		if n != 1 {
			t.Errorf("prefix %q returned %d times across pages, want exactly 1", p, n)
		}
	}

	for _, want := range []string{"d1/", "d2/", "d3/", "d4/"} {
		if seenPrefixes[want] == 0 {
			t.Errorf("prefix %q was never returned", want)
		}
	}
	if seenKeys["aaa.txt"] == 0 {
		t.Error("object aaa.txt was never returned")
	}
}

// TestListObjectsPaginationCoversAllKeys walks a flat bucket in small pages and
// asserts every key appears exactly once.
func TestListObjectsPaginationCoversAllKeys(t *testing.T) {
	var keys []string
	for i := 0; i < 25; i++ {
		keys = append(keys, fmt.Sprintf("obj-%02d.txt", i))
	}
	fs := newListEngine(t, "flat", keys...)

	seen := map[string]int{}
	token := ""
	for page := 0; page < 20; page++ {
		out, err := fs.ListObjects(&ListObjectsInput{
			Bucket:            "flat",
			MaxKeys:           4,
			ContinuationToken: token,
		})
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, o := range out.Objects {
			seen[o.Key]++
		}
		if !out.IsTruncated {
			break
		}
		token = out.NextContinuationToken
	}

	if len(seen) != len(keys) {
		t.Errorf("saw %d distinct keys, want %d", len(seen), len(keys))
	}
	for _, k := range keys {
		if seen[k] != 1 {
			t.Errorf("key %q seen %d times, want 1", k, seen[k])
		}
	}
}

// TestListObjectVersionsEnumeratesHistory covers the newly added
// ListObjectVersions surface: versioning existed in storage but had no API.
func TestListObjectVersionsEnumeratesHistory(t *testing.T) {
	fs := newListEngine(t, "versioned")
	if err := fs.SetBucketVersioning("versioned", "Enabled"); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := fs.PutObject(context.Background(), "versioned", "file.txt",
			strings.NewReader(fmt.Sprintf("v%d", i)), 2, "text/plain"); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	out, err := fs.ListObjectVersions(&ListVersionsInput{Bucket: "versioned"})
	if err != nil {
		t.Fatalf("ListObjectVersions: %v", err)
	}
	if len(out.Versions) != 3 {
		t.Errorf("got %d versions, want 3", len(out.Versions))
	}

	latest := 0
	ids := map[string]bool{}
	for _, v := range out.Versions {
		if v.IsLatest {
			latest++
		}
		if ids[v.VersionID] {
			t.Errorf("duplicate version ID %q", v.VersionID)
		}
		ids[v.VersionID] = true
	}
	if latest != 1 {
		t.Errorf("got %d versions flagged IsLatest, want exactly 1", latest)
	}
}

// TestListObjectVersionsIncludesDeleteMarkers ensures delete markers are
// reported separately from live versions.
func TestListObjectVersionsIncludesDeleteMarkers(t *testing.T) {
	fs := newListEngine(t, "versioned")
	if err := fs.SetBucketVersioning("versioned", "Enabled"); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}
	if _, err := fs.PutObject(context.Background(), "versioned", "gone.txt",
		strings.NewReader("x"), 1, "text/plain"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, _, err := fs.DeleteObject("versioned", "gone.txt", ""); err != nil {
		t.Fatalf("delete: %v", err)
	}

	out, err := fs.ListObjectVersions(&ListVersionsInput{Bucket: "versioned"})
	if err != nil {
		t.Fatalf("ListObjectVersions: %v", err)
	}
	if len(out.DeleteMarkers) != 1 {
		t.Errorf("got %d delete markers, want 1", len(out.DeleteMarkers))
	}
	if len(out.Versions) != 1 {
		t.Errorf("got %d live versions, want 1", len(out.Versions))
	}
}

// TestListObjectVersionsPaginatesAcrossKeysWithoutDuplicatesOrGaps exercises
// the cursor-based rewrite of ListObjectVersions across many keys, each with
// several versions, paging with a small MaxKeys so multiple pages are
// required. It guards both the pagination contract (NextKeyMarker/
// NextVersionIDMarker must let the next call resume exactly where the last
// one stopped, with no duplicate or skipped entries) and per-key ordering
// (newest version first) — the property the previous full-sort-in-memory
// implementation got from a single global sort, which the per-key cursor
// gather-and-sort here must reproduce without ever materializing the whole
// bucket's version history.
func TestListObjectVersionsPaginatesAcrossKeysWithoutDuplicatesOrGaps(t *testing.T) {
	fs := newListEngine(t, "versioned-page")
	if err := fs.SetBucketVersioning("versioned-page", "Enabled"); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}

	keys := []string{"a.txt", "b.txt", "c.txt", "d.txt"}
	const versionsPerKey = 3
	for _, key := range keys {
		for i := 0; i < versionsPerKey; i++ {
			if _, err := fs.PutObject(context.Background(), "versioned-page", key,
				strings.NewReader(fmt.Sprintf("%s-v%d", key, i)), int64(len(key)+3), "text/plain"); err != nil {
				t.Fatalf("put %s v%d: %v", key, i, err)
			}
		}
	}

	type seenEntry struct {
		key, versionID string
	}
	var all []seenEntry
	seen := make(map[seenEntry]bool)

	keyMarker, versionIDMarker := "", ""
	pages := 0
	for {
		pages++
		if pages > 50 {
			t.Fatal("pagination did not terminate — likely stuck repeating a page")
		}
		out, err := fs.ListObjectVersions(&ListVersionsInput{
			Bucket:          "versioned-page",
			MaxKeys:         2,
			KeyMarker:       keyMarker,
			VersionIDMarker: versionIDMarker,
		})
		if err != nil {
			t.Fatalf("ListObjectVersions page %d: %v", pages, err)
		}

		// Within this page, versions of the same key must appear newest
		// first (descending LastModified).
		for i, v := range out.Versions {
			if i > 0 && v.Key == out.Versions[i-1].Key {
				if v.LastModified.After(out.Versions[i-1].LastModified) {
					t.Errorf("page %d: versions of %q not newest-first: %v before %v",
						pages, v.Key, out.Versions[i-1].LastModified, v.LastModified)
				}
			}
			entry := seenEntry{v.Key, v.VersionID}
			if seen[entry] {
				t.Fatalf("page %d: duplicate entry %+v across pages", pages, entry)
			}
			seen[entry] = true
			all = append(all, entry)
		}

		if !out.IsTruncated {
			break
		}
		if out.NextKeyMarker == "" {
			t.Fatalf("page %d: truncated but NextKeyMarker is empty", pages)
		}
		keyMarker = out.NextKeyMarker
		versionIDMarker = out.NextVersionIDMarker
	}

	if len(all) != len(keys)*versionsPerKey {
		t.Fatalf("collected %d version entries across %d pages, want %d", len(all), pages, len(keys)*versionsPerKey)
	}

	// Global key ordering across pages must be ascending, matching S3's
	// contract and what a single-shot (non-paginated) listing would return.
	for i := 1; i < len(all); i++ {
		if all[i].key < all[i-1].key {
			t.Errorf("keys out of order across pages: %q came after %q", all[i].key, all[i-1].key)
		}
	}
}

// TestListPartsReturnsUploadedParts covers the new ListParts surface.
func TestListPartsReturnsUploadedParts(t *testing.T) {
	fs := newListEngine(t, "mpu")
	up, err := fs.CreateMultipartUpload("mpu", "big.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("create mpu: %v", err)
	}

	for _, n := range []int{2, 1, 3} {
		body := strings.Repeat("z", 16)
		if _, err := fs.UploadPart(context.Background(), "mpu", "big.bin", up.UploadID, n,
			strings.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("upload part %d: %v", n, err)
		}
	}

	parts, err := fs.ListParts("mpu", "big.bin", up.UploadID)
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("got %d parts, want 3", len(parts))
	}
	for i, p := range parts {
		if p.PartNumber != i+1 {
			t.Errorf("part %d has number %d, want ascending order", i, p.PartNumber)
		}
	}
}

// TestListMultipartUploadsReportsInFlight covers the new ListMultipartUploads
// surface, which clients need to discover and reclaim abandoned uploads.
func TestListMultipartUploadsReportsInFlight(t *testing.T) {
	fs := newListEngine(t, "mpu")
	for _, key := range []string{"b.bin", "a.bin"} {
		if _, err := fs.CreateMultipartUpload("mpu", key, "application/octet-stream"); err != nil {
			t.Fatalf("create mpu %s: %v", key, err)
		}
	}

	uploads, truncated, err := fs.ListMultipartUploads("mpu", "", "", "", 100)
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	if truncated {
		t.Error("unexpectedly truncated")
	}
	if len(uploads) != 2 {
		t.Fatalf("got %d uploads, want 2", len(uploads))
	}
	if uploads[0].Key != "a.bin" || uploads[1].Key != "b.bin" {
		t.Errorf("uploads not sorted by key: %q, %q", uploads[0].Key, uploads[1].Key)
	}
}

// TestListMultipartUploadsPaginationAdvances guards against a gap where
// key-marker/upload-id-marker were accepted but never acted on: a caller
// paging through more in-progress uploads than fit in one response got the
// exact same first page back every time, regardless of the markers it sent.
func TestListMultipartUploadsPaginationAdvances(t *testing.T) {
	fs := newListEngine(t, "mpu-page")
	keys := []string{"a.bin", "b.bin", "c.bin", "d.bin", "e.bin"}
	for _, key := range keys {
		if _, err := fs.CreateMultipartUpload("mpu-page", key, "application/octet-stream"); err != nil {
			t.Fatalf("create mpu %s: %v", key, err)
		}
	}

	page1, truncated, err := fs.ListMultipartUploads("mpu-page", "", "", "", 2)
	if err != nil {
		t.Fatalf("ListMultipartUploads page1: %v", err)
	}
	if !truncated {
		t.Fatal("expected page1 to be truncated")
	}
	if len(page1) != 2 || page1[0].Key != "a.bin" || page1[1].Key != "b.bin" {
		t.Fatalf("unexpected page1: %+v", page1)
	}

	last := page1[len(page1)-1]
	page2, truncated, err := fs.ListMultipartUploads("mpu-page", "", last.Key, last.UploadID, 2)
	if err != nil {
		t.Fatalf("ListMultipartUploads page2: %v", err)
	}
	if !truncated {
		t.Fatal("expected page2 to be truncated")
	}
	if len(page2) != 2 || page2[0].Key != "c.bin" || page2[1].Key != "d.bin" {
		t.Fatalf("page2 did not advance past the marker (pagination stuck repeating page1?): %+v", page2)
	}

	last = page2[len(page2)-1]
	page3, truncated, err := fs.ListMultipartUploads("mpu-page", "", last.Key, last.UploadID, 2)
	if err != nil {
		t.Fatalf("ListMultipartUploads page3: %v", err)
	}
	if truncated {
		t.Error("expected page3 to be the final page")
	}
	if len(page3) != 1 || page3[0].Key != "e.bin" {
		t.Fatalf("unexpected page3: %+v", page3)
	}
}

// TestListObjectsPrefixWithEarlierMarker is the regression test for a listing
// bug that returned an empty page.
//
// When StartAfter (or the V1 marker) was set, the seek key was built from it
// alone and Prefix was ignored. The cursor therefore landed wherever the marker
// pointed — possibly before the prefix — and the loop's prefix check breaks on
// the first non-matching key, so the scan ended immediately and reported no
// objects at all even though matching keys existed further on.
func TestListObjectsPrefixWithEarlierMarker(t *testing.T) {
	fs := newListEngine(t, "testbucket",
		"apple.txt", "banana.txt", "photos/a.jpg", "photos/b.jpg", "zebra.txt")

	out, err := fs.ListObjects(&ListObjectsInput{
		Bucket:     "testbucket",
		Prefix:     "photos/",
		StartAfter: "a", // sorts before the prefix
		MaxKeys:    100,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var got []string
	for _, o := range out.Objects {
		got = append(got, o.Key)
	}
	want := []string{"photos/a.jpg", "photos/b.jpg"}
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
}

// TestListObjectsPrefixWithMarkerInsideRange covers the other half: a marker
// that sorts *after* the prefix must still skip what it names, rather than the
// prefix seek dragging the cursor back to the start of the range.
func TestListObjectsPrefixWithMarkerInsideRange(t *testing.T) {
	fs := newListEngine(t, "testbucket",
		"photos/a.jpg", "photos/b.jpg", "photos/c.jpg")

	out, err := fs.ListObjects(&ListObjectsInput{
		Bucket:     "testbucket",
		Prefix:     "photos/",
		StartAfter: "photos/a.jpg",
		MaxKeys:    100,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	var got []string
	for _, o := range out.Objects {
		got = append(got, o.Key)
	}
	want := []string{"photos/b.jpg", "photos/c.jpg"}
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
}

// TestListObjectsPrefixWithEarlierMarkerAndDelimiter exercises the same seek
// logic on the delimiter path, where common prefixes are rolled up.
func TestListObjectsPrefixWithEarlierMarkerAndDelimiter(t *testing.T) {
	fs := newListEngine(t, "testbucket",
		"apple.txt", "photos/2023/a.jpg", "photos/2024/b.jpg")

	out, err := fs.ListObjects(&ListObjectsInput{
		Bucket:     "testbucket",
		Prefix:     "photos/",
		Delimiter:  "/",
		StartAfter: "a",
		MaxKeys:    100,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	want := []string{"photos/2023/", "photos/2024/"}
	if len(out.CommonPrefixes) != len(want) {
		t.Fatalf("common prefixes = %v, want %v", out.CommonPrefixes, want)
	}
	for i := range want {
		if out.CommonPrefixes[i] != want[i] {
			t.Fatalf("common prefixes = %v, want %v", out.CommonPrefixes, want)
		}
	}
}
