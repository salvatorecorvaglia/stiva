package storage

import (
	"errors"
	"log/slog"
	"time"
)

// Expiration checks and mutation share the publication lock, so stale scans
// cannot delete a replacement or a version that became current again.
func (fs *FilesystemEngine) expireCurrentVersion(bucket, key, versionID string, modified time.Time) error {
	unlock := fs.lockObjectKey(bucket, key)
	defer unlock()
	current, err := fs.metadata.GetObjectMeta(bucket, key, "")
	if errors.Is(err, errObjectMetaNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.VersionID != versionID || !current.LastModified.Equal(modified) {
		return nil
	}
	if current.IsDeleteMarker {
		versions, err := fs.metadata.GetObjectVersions(bucket, key)
		if err != nil {
			return err
		}
		for _, version := range versions {
			if version.VersionID != versionID {
				return nil
			}
		}
	} else {
		versionID = ""
	}
	_, _, err = fs.deleteObjectLocked(bucket, key, versionID)
	return err
}

func (fs *FilesystemEngine) expireNoncurrentVersion(bucket, key, versionID string, now time.Time, cutoff time.Duration) error {
	unlock := fs.lockObjectKey(bucket, key)
	defer unlock()
	candidate, err := fs.metadata.GetObjectMeta(bucket, key, versionID)
	if errors.Is(err, errObjectMetaNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if candidate.IsLatest {
		return nil
	}
	since := candidate.NoncurrentSince
	if since.IsZero() {
		// Old metadata predates NoncurrentSince. Only an identifiable successor
		// supplies a safe retention start; ambiguous records are retained.
		versions, err := fs.metadata.GetObjectVersions(bucket, key)
		if err != nil {
			return err
		}
		for _, successor := range versions {
			if successor.LastModified.After(candidate.LastModified) && (since.IsZero() || successor.LastModified.Before(since)) {
				since = successor.LastModified
			}
		}
	}
	if since.IsZero() || now.Sub(since) <= cutoff {
		return nil
	}
	slog.Info("[Storage] Expiring noncurrent version", "bucket", bucket, "key", key, "versionID", versionID, "noncurrentSince", since)
	_, _, err = fs.deleteObjectLocked(bucket, key, versionID)
	return err
}
