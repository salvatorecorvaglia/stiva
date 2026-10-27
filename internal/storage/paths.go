package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (fs *FilesystemEngine) bucketPath(name string) string {
	return filepath.Join(fs.dataDir, "buckets", name)
}

// maxKeyLength is the S3 limit on an object key, in bytes.
const maxKeyLength = 1024

// Object data layout.
//
// Each object version is stored in its own file, named by a random blob ID:
//
//	<dataDir>/blobs/<bucket>/<id[0:2]>/<id[2:4]>/<id>
//
// and its metadata records the ID. Object keys used to be paths on the
// filesystem (<dataDir>/buckets/<bucket>/<key>[.<versionID>]), which meant a
// key could not be both a file and a directory ("a" and "a/b", or a "photos/"
// folder marker), keys differing only in case overwrote each other on
// case-insensitive filesystems, a key segment could not exceed a filename's
// length, and "<key>.<versionID>" could collide with a real key. Objects
// written that way have no blob ID and are still read from their old path;
// they move to the blob layout when overwritten.

// blobDir is where a bucket's object data lives in the blob layout.
func (fs *FilesystemEngine) blobDir(bucket string) string {
	return filepath.Join(fs.dataDir, "blobs", bucket)
}

// newBlobID returns a random 128-bit blob ID as 32 hex digits.
func newBlobID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("failed to generate blob ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// isBlobID reports whether id has the form newBlobID produces, so a blob ID
// read from metadata can never steer a path outside the blob directory.
func isBlobID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (fs *FilesystemEngine) blobPath(bucket, id string) string {
	return filepath.Join(fs.blobDir(bucket), id[0:2], id[2:4], id)
}

// dataPath returns where an object version's data is stored: its blob, or,
// for data written before the blob layout, its old key-derived path.
func (fs *FilesystemEngine) dataPath(info *ObjectInfo) (string, error) {
	if info.BlobID == "" {
		return fs.objectPathWithVersion(info.Bucket, info.Key, info.VersionID)
	}
	if !isBlobID(info.BlobID) {
		return "", fmt.Errorf("invalid blob ID %q in metadata", info.BlobID)
	}
	return fs.blobPath(info.Bucket, info.BlobID), nil
}

// newBlob allocates a blob for a new object version and creates its
// directory, returning the blob's ID and path.
func (fs *FilesystemEngine) newBlob(bucket string) (id, path string, err error) {
	if err := fs.validateBucketName(bucket); err != nil {
		return "", "", err
	}
	id, err = newBlobID()
	if err != nil {
		return "", "", err
	}
	path = fs.blobPath(bucket, id)
	root, err := os.OpenRoot(fs.dataDir)
	if err != nil {
		return "", "", err
	}
	defer root.Close()
	rel := filepath.Join("blobs", bucket, id[:2], id[2:4])
	if err := root.MkdirAll(rel, 0755); err != nil {
		return "", "", fmt.Errorf("failed to create blob directory: %w", err)
	}
	return id, path, nil
}

// validateNewKey checks a key an object is about to be written under.
//
// Both rules used to hold only as side effects of keys being filesystem
// paths. Length: the S3 limit is 1,024 bytes. NUL: metadata keys join the
// bucket, key and version ID with NUL bytes, so a key containing one could
// alias another key's version records ("a\x00v1" is version v1 of "a");
// filenames cannot contain NUL, which is the only reason that was never
// reachable before.
func validateNewKey(key string) error {
	if len(key) > maxKeyLength {
		return &S3Error{Code: "KeyTooLongError", Message: "Your key is too long."}
	}
	if strings.IndexByte(key, 0) >= 0 {
		return &S3Error{Code: "InvalidArgument", Message: "Object keys cannot contain NUL characters."}
	}
	return nil
}

func hasPathTraversal(s string) bool {
	if s == ".." || strings.HasPrefix(s, "../") || strings.HasPrefix(s, "..\\") || strings.HasSuffix(s, "/..") || strings.HasSuffix(s, "\\..") {
		return true
	}
	if strings.Contains(s, "/../") || strings.Contains(s, "\\..\\") || strings.Contains(s, "/..\\") || strings.Contains(s, "\\../") {
		return true
	}
	return false
}

func (fs *FilesystemEngine) validatePathSafety(bucket, key, versionID string) error {
	if !filepath.IsLocal(bucket) || hasPathTraversal(bucket) {
		return errors.New(errInvalidBucketTraversal)
	}
	if hasPathTraversal(key) {
		return errors.New(errInvalidKeyTraversal)
	}
	normalizedKey := strings.ReplaceAll(key, "\\", "/")
	trimmedKey := strings.TrimLeft(normalizedKey, "/")
	if trimmedKey != "" {
		if !filepath.IsLocal(filepath.FromSlash(trimmedKey)) || hasPathTraversal(trimmedKey) {
			return errors.New(errInvalidKeyTraversal)
		}
	} else if key != "" {
		return errors.New(errInvalidKeyTraversal)
	}
	if versionID != "" {
		if !filepath.IsLocal(versionID) || hasPathTraversal(versionID) || strings.ContainsAny(versionID, "/\\") {
			return fmt.Errorf("invalid version ID: path traversal detected")
		}
	}
	return nil
}

func (fs *FilesystemEngine) objectPath(bucket, key string) (string, error) {
	if err := fs.validatePathSafety(bucket, key, ""); err != nil {
		return "", err
	}

	normalizedKey := strings.ReplaceAll(key, "\\", "/")
	base := filepath.Clean(fs.bucketPath(bucket))
	resolved := filepath.Clean(filepath.Join(base, filepath.FromSlash(normalizedKey)))
	// Ensure the resolved path stays within the bucket directory
	rel, err := filepath.Rel(base, resolved)
	if err != nil || filepath.IsAbs(rel) || isRelTraversal(rel) {
		return "", errors.New(errInvalidKeyTraversal)
	}
	basePrefix := base + string(filepath.Separator)
	if resolved != base && !strings.HasPrefix(resolved, basePrefix) {
		return "", errors.New(errInvalidKeyTraversal)
	}
	return resolved, nil
}

func (fs *FilesystemEngine) objectPathWithVersion(bucket, key, versionID string) (string, error) {
	if err := fs.validatePathSafety(bucket, key, versionID); err != nil {
		return "", err
	}
	base, err := fs.objectPath(bucket, key)
	if err != nil {
		return "", err
	}
	resolved := base
	if versionID != "" {
		resolved = base + "." + versionID
	}
	resolved = filepath.Clean(resolved)
	// Ensure the resolved path stays within the bucket directory
	bucketBase := filepath.Clean(fs.bucketPath(bucket))
	rel, err := filepath.Rel(bucketBase, resolved)
	if err != nil || filepath.IsAbs(rel) || isRelTraversal(rel) {
		return "", fmt.Errorf("invalid object key or version: path traversal detected")
	}
	basePrefix := bucketBase + string(filepath.Separator)
	if resolved != bucketBase && !strings.HasPrefix(resolved, basePrefix) {
		return "", fmt.Errorf("invalid object key or version: path traversal detected")
	}
	return resolved, nil
}

func (fs *FilesystemEngine) cleanupParentDirs(objPath string, bucket string) {
	if !filepath.IsLocal(bucket) || hasPathTraversal(bucket) {
		return
	}
	bucketDir := filepath.Clean(fs.bucketPath(bucket))
	cleanObjPath := filepath.Clean(objPath)
	bucketPrefix := bucketDir + string(filepath.Separator)
	if !strings.HasPrefix(cleanObjPath, bucketPrefix) {
		return
	}
	rel, err := filepath.Rel(bucketDir, cleanObjPath)
	if err != nil || filepath.IsAbs(rel) || isRelTraversal(rel) {
		return
	}

	dir := filepath.Dir(cleanObjPath)
	for dir != bucketDir {
		cleanDir := filepath.Clean(dir)
		if !strings.HasPrefix(cleanDir, bucketPrefix) {
			break
		}
		checkRel, err := filepath.Rel(bucketDir, cleanDir)
		if err != nil || filepath.IsAbs(checkRel) || isRelTraversal(checkRel) {
			break
		}

		entries, err := os.ReadDir(cleanDir)
		if err != nil || len(entries) > 0 {
			break
		}
		if err := os.Remove(cleanDir); err != nil {
			break
		}
		parent := filepath.Dir(cleanDir)
		if parent == cleanDir {
			break
		}
		dir = parent
	}
}

func (fs *FilesystemEngine) multipartUploadPath(bucket, uploadID string) (string, error) {
	if !filepath.IsLocal(bucket) || hasPathTraversal(bucket) {
		return "", errors.New(errInvalidBucketTraversal)
	}
	if !filepath.IsLocal(uploadID) || hasPathTraversal(uploadID) || strings.ContainsAny(uploadID, "/\\") {
		return "", errors.New(errInvalidUploadID)
	}
	base := filepath.Clean(filepath.Join(fs.dataDir, "multipart", bucket))
	resolved := filepath.Clean(filepath.Join(base, uploadID))
	// Ensure the resolved path stays within the bucket's multipart directory
	rel, err := filepath.Rel(base, resolved)
	if err != nil || filepath.IsAbs(rel) || isRelTraversal(rel) {
		return "", errors.New(errInvalidUploadID)
	}
	basePrefix := base + string(filepath.Separator)
	if resolved != base && !strings.HasPrefix(resolved, basePrefix) {
		return "", errors.New(errInvalidUploadID)
	}
	return resolved, nil
}

func (fs *FilesystemEngine) multipartDir(bucket, key, uploadID string) (string, error) {
	if !filepath.IsLocal(bucket) || hasPathTraversal(bucket) {
		return "", errors.New(errInvalidBucketTraversal)
	}
	if hasPathTraversal(key) {
		return "", errors.New(errInvalidKeyTraversal)
	}
	trimmedKey := strings.TrimLeft(key, "/\\")
	if trimmedKey != "" {
		if !filepath.IsLocal(filepath.FromSlash(trimmedKey)) || hasPathTraversal(trimmedKey) {
			return "", errors.New(errInvalidKeyTraversal)
		}
	} else if key != "" {
		return "", errors.New(errInvalidKeyTraversal)
	}
	if !filepath.IsLocal(uploadID) || hasPathTraversal(uploadID) {
		return "", errors.New(errInvalidUploadID)
	}
	uploadPath, err := fs.multipartUploadPath(bucket, uploadID)
	if err != nil {
		return "", err
	}
	cleanUploadPath := filepath.Clean(uploadPath)
	resolved := filepath.Clean(filepath.Join(cleanUploadPath, filepath.FromSlash(key)))
	// Ensure the resolved path stays within the uploadPath directory
	rel, err := filepath.Rel(cleanUploadPath, resolved)
	if err != nil || filepath.IsAbs(rel) || isRelTraversal(rel) {
		return "", errors.New(errInvalidKeyTraversal)
	}
	uploadPrefix := cleanUploadPath + string(filepath.Separator)
	if resolved != cleanUploadPath && !strings.HasPrefix(resolved, uploadPrefix) {
		return "", errors.New(errInvalidKeyTraversal)
	}
	return resolved, nil
}

func isRelTraversal(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "..\\") || hasPathTraversal(rel)
}

// requireBucket checks a bucket name is valid and the bucket exists: the
// preamble of nearly every bucket-scoped engine operation.
func (fs *FilesystemEngine) requireBucket(name string) error {
	if err := fs.validateBucketName(name); err != nil {
		return err
	}
	return fs.bucketMustExist(name)
}

// bucketMustExist reports NoSuchBucket for a bucket that does not exist. It is
// requireBucket without the name check, for operations that must run other
// checks in between to report errors in S3's order.
func (fs *FilesystemEngine) bucketMustExist(name string) error {
	exists, err := fs.metadata.BucketExists(name)
	if err != nil {
		return err
	}
	if !exists {
		return &S3Error{Code: "NoSuchBucket", Message: errBucketNotFound}
	}
	return nil
}

func (fs *FilesystemEngine) validateBucketName(name string) error {
	if !IsValidBucketName(name) {
		return &S3Error{Code: "InvalidBucketName", Message: "The specified bucket is not valid."}
	}
	return nil
}
