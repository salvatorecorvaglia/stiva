package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	bolt "go.etcd.io/bbolt"
)

// errListPageFull stops a bbolt cursor scan early once a page is full. It
// never escapes this file — db.View() callers translate it back to nil.
var errListPageFull = errors.New("list page full")

// ListVersionsInput holds parameters for enumerating object versions.
type ListVersionsInput struct {
	Bucket          string
	Prefix          string
	Delimiter       string
	MaxKeys         int
	KeyMarker       string
	VersionIDMarker string
}

// ListVersionsOutput holds the result of enumerating object versions.
type ListVersionsOutput struct {
	Versions            []ObjectInfo
	DeleteMarkers       []ObjectInfo
	CommonPrefixes      []string
	IsTruncated         bool
	NextKeyMarker       string
	NextVersionIDMarker string
}

// ListObjectVersions enumerates every version of every object in a bucket,
// separating live versions from delete markers.
//
// Versioning was already implemented in the metadata layer but had no API
// surface, so clients had no way to enumerate or reclaim old versions.
//
// This walks the bbolt cursor directly (like ListObjects) rather than
// collecting every version of every key into memory and sorting the whole
// thing: a heavily versioned bucket made every page request O(total
// versions) in both memory and CPU regardless of MaxKeys. Object keys are
// already stored in sorted order, so the cursor only needs to gather and
// sort the versions of one key at a time — bounded by that key's own
// version count, not the bucket's — and can stop entirely once a page is
// full.
func (fs *FilesystemEngine) ListObjectVersions(input *ListVersionsInput) (*ListVersionsOutput, error) {
	if err := fs.requireBucket(input.Bucket); err != nil {
		return nil, err
	}

	maxKeys := input.MaxKeys
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	out := &ListVersionsOutput{}
	count := 0
	// lastKey/lastVersionID track the most recently emitted entry (a version,
	// delete marker, or common prefix) so a page's NextKeyMarker points at
	// something the caller already received — not at the entry that didn't
	// fit, which would then be skipped on the next page and silently lost.
	var lastKey, lastVersionID string

	db, releasedb, err := fs.metadata.acquireBucketDB(input.Bucket)
	if err != nil {
		return nil, err
	}
	defer releasedb()

	bucketPrefix := input.Bucket + "\x00"
	bucketPrefixBytes := []byte(bucketPrefix)

	// A key marker ending in the delimiter is a rolled-up common prefix — what
	// NextKeyMarker is when a page ends on one. Resuming from it must skip every
	// key under that prefix, or the prefix is emitted again on the next page.
	markerIsPrefix := input.KeyMarker != "" && input.Delimiter != "" &&
		strings.HasSuffix(input.KeyMarker, input.Delimiter)

	// Start at whichever of the prefix and the marker sorts later. The marker
	// used to win outright, so a marker sorting before the prefix started the
	// scan outside it and the loop stopped at the first non-matching key.
	start := input.Prefix
	if input.KeyMarker != "" {
		resume := input.KeyMarker
		if markerIsPrefix {
			resume += "\xff" // no UTF-8 key contains 0xff, so this sorts after every key under the prefix
		}
		start = advanceToken(start, resume)
	}

	// objectKeyOf splits a raw bbolt key (already known to carry
	// bucketPrefix) into just the object key, whether it's the "latest
	// pointer" entry (bucket\x00key) or a historical version entry
	// (bucket\x00key\x00versionID).
	objectKeyOf := func(raw []byte) string {
		rest := string(raw[len(bucketPrefixBytes):])
		if idx := strings.IndexByte(rest, 0); idx >= 0 {
			return rest[:idx]
		}
		return rest
	}

	pageFull := func() error {
		out.IsTruncated = true
		out.NextKeyMarker = lastKey
		out.NextVersionIDMarker = lastVersionID
		return errListPageFull
	}

	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		if b == nil {
			return nil
		}
		c := b.Cursor()

		k, v := c.Seek([]byte(bucketPrefix + start))
		for k != nil && bytes.HasPrefix(k, bucketPrefixBytes) {
			objectKey := objectKeyOf(k)
			if !strings.HasPrefix(objectKey, input.Prefix) {
				break
			}

			// Roll keys below a delimiter up into a common prefix, then jump
			// past every key under it: they would all roll up the same way.
			if input.Delimiter != "" {
				remaining := objectKey[len(input.Prefix):]
				if idx := strings.Index(remaining, input.Delimiter); idx >= 0 {
					dirPrefix := input.Prefix + remaining[:idx+len(input.Delimiter)]
					if count >= maxKeys {
						return pageFull()
					}
					out.CommonPrefixes = append(out.CommonPrefixes, dirPrefix)
					lastKey, lastVersionID = dirPrefix, ""
					count++
					k, v = c.Seek([]byte(bucketPrefix + dirPrefix + "\xff"))
					continue
				}
			}

			// Gather every version of this one key — typically a handful,
			// unlike the bucket as a whole — so it can be sorted into the
			// newest-first order S3 requires.
			var keyVersions []ObjectInfo
			seenVer := make(map[string]bool)
			for k != nil && bytes.HasPrefix(k, bucketPrefixBytes) && objectKeyOf(k) == objectKey {
				var info ObjectInfo
				if err := json.Unmarshal(v, &info); err != nil {
					return err
				}
				if !seenVer[info.VersionID] {
					seenVer[info.VersionID] = true
					keyVersions = append(keyVersions, info)
				}
				k, v = c.Next()
			}
			sort.SliceStable(keyVersions, func(i, j int) bool {
				return keyVersions[i].LastModified.After(keyVersions[j].LastModified)
			})

			if objectKey == input.KeyMarker && !markerIsPrefix {
				keyVersions = versionsAfterMarker(keyVersions, input.VersionIDMarker)
			}

			for _, info := range keyVersions {
				if count >= maxKeys {
					return pageFull()
				}
				if info.IsDeleteMarker {
					out.DeleteMarkers = append(out.DeleteMarkers, info)
				} else {
					out.Versions = append(out.Versions, info)
				}
				lastKey, lastVersionID = info.Key, markerVersionID(info.VersionID)
				count++
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errListPageFull) {
		return nil, err
	}

	return out, nil
}

// versionsAfterMarker returns the versions of the marker key (newest first)
// that come after versionIDMarker.
//
// With no version marker, S3 resumes after the whole key; this used to treat
// the key's first version as the marker and re-emit the rest. If the marker
// version has since been deleted, its position within the key is unknown, so
// the listing moves on to the next key rather than — as it used to — never
// finding the marker and returning an empty, untruncated page that ended
// pagination early.
func versionsAfterMarker(versions []ObjectInfo, versionIDMarker string) []ObjectInfo {
	if versionIDMarker == "" {
		return nil
	}
	for i, info := range versions {
		if markerVersionID(info.VersionID) == versionIDMarker {
			return versions[i+1:]
		}
	}
	return nil
}

// markerVersionID is the version ID as clients see it in listings and send
// back as version-id-marker: an unversioned object is reported as "null".
func markerVersionID(versionID string) string {
	if versionID == "" {
		return "null"
	}
	return versionID
}

// ListMultipartUploads returns the in-progress multipart uploads for a bucket,
// ordered by key then upload ID. keyMarker/uploadIDMarker resume listing
// after that (key, uploadID) pair, matching the NextKeyMarker/
// NextUploadIdMarker a truncated response returns — without this, a caller
// paging through more uploads than fit in one response got the same first
// page back every time, since nothing here previously acted on the markers.
//
// Multipart metadata keys are already stored as bucket\x00key\x00uploadID,
// which sorts in exactly the order this needs, so — like ListObjects — a
// direct cursor scan produces the right order and can stop as soon as a page
// is full, instead of loading every in-progress upload in the bucket into
// memory just to sort and slice it.
func (fs *FilesystemEngine) ListMultipartUploads(bucket, prefix, keyMarker, uploadIDMarker string, maxUploads int) ([]MultipartUploadInfo, bool, error) {
	if err := fs.requireBucket(bucket); err != nil {
		return nil, false, err
	}

	if maxUploads <= 0 {
		maxUploads = 1000
	}

	db, releasedb, err := fs.metadata.acquireBucketDB(bucket)
	if err != nil {
		return nil, false, err
	}
	defer releasedb()

	bucketPrefix := bucket + "\x00"
	bucketPrefixBytes := []byte(bucketPrefix)

	seekKey := bucketPrefix
	switch {
	case keyMarker != "":
		seekKey = bucketPrefix + keyMarker + "\x00" + uploadIDMarker
	case prefix != "":
		seekKey = bucketPrefix + prefix
	}

	var uploads []MultipartUploadInfo
	truncated := false

	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(multipartBucket)
		if b == nil {
			return nil
		}
		c := b.Cursor()

		k, v := c.Seek([]byte(seekKey))
		for k != nil && bytes.HasPrefix(k, bucketPrefixBytes) {
			var meta MultipartMeta
			if err := json.Unmarshal(v, &meta); err != nil {
				return err
			}

			if prefix != "" && !strings.HasPrefix(meta.Key, prefix) {
				break
			}

			// Skip the marker entry itself; the caller already has it from
			// the previous page.
			if keyMarker != "" && meta.Key == keyMarker && meta.UploadID == uploadIDMarker {
				k, v = c.Next()
				continue
			}

			if len(uploads) >= maxUploads {
				truncated = true
				return nil
			}

			uploads = append(uploads, MultipartUploadInfo{
				UploadID: meta.UploadID,
				Bucket:   meta.Bucket,
				Key:      meta.Key,
				Created:  meta.Created,
			})
			k, v = c.Next()
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}

	return uploads, truncated, nil
}

// ListParts returns the parts uploaded so far for a multipart upload, ordered by
// part number.
func (fs *FilesystemEngine) ListParts(bucket, key, uploadID string) ([]PartInfo, error) {
	if err := fs.validateBucketName(bucket); err != nil {
		return nil, err
	}
	if _, err := fs.objectPath(bucket, key); err != nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}

	meta, err := fs.metadata.GetMultipartMeta(bucket, key, uploadID)
	if err != nil {
		return nil, &S3Error{Code: "NoSuchUpload", Message: errUploadNotFound}
	}

	parts := make([]PartInfo, len(meta.Parts))
	copy(parts, meta.Parts)
	sort.Slice(parts, func(i, j int) bool {
		return parts[i].PartNumber < parts[j].PartNumber
	})
	return parts, nil
}
