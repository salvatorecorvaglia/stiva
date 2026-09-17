package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (fs *FilesystemEngine) bucketPath(name string) string {
	return filepath.Join(fs.dataDir, "buckets", name)
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

func (fs *FilesystemEngine) checkPathConflict(objPath string, bucket string) error {
	bucketDir := filepath.Clean(fs.bucketPath(bucket))
	cleanObjPath := filepath.Clean(objPath)
	bucketPrefix := bucketDir + string(filepath.Separator)
	if !strings.HasPrefix(cleanObjPath, bucketPrefix) {
		return errors.New(errInvalidKeyTraversal)
	}
	rel, err := filepath.Rel(bucketDir, cleanObjPath)
	if err != nil || filepath.IsAbs(rel) || isRelTraversal(rel) {
		return errors.New(errInvalidKeyTraversal)
	}

	// Check if the proposed object path is already a directory (file-directory conflict)
	fi, err := os.Stat(cleanObjPath)
	if err == nil && fi.IsDir() {
		return &S3Error{Code: "InvalidRequest", Message: "Object key name conflicts with an existing directory path."}
	}

	// Check if any parent path is a regular file (directory-file conflict)
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

		s, err := os.Stat(cleanDir)
		if err == nil && !s.IsDir() {
			return &S3Error{Code: "InvalidRequest", Message: "Parent path conflicts with an existing object."}
		}

		parent := filepath.Dir(cleanDir)
		if parent == cleanDir {
			break
		}
		dir = parent
	}
	// A Stat error above means the path simply isn't there, which is exactly
	// the no-conflict case this reports.
	return nil //nolint:nilerr // intentional: a missing parent path is not a conflict
}

func (fs *FilesystemEngine) validateBucketName(name string) error {
	if !IsValidBucketName(name) {
		return &S3Error{Code: "InvalidBucketName", Message: "The specified bucket is not valid."}
	}
	return nil
}
