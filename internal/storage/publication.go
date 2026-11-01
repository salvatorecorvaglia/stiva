package storage

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// commitObject publishes a fully written temp file as the data of info, a
// new version of info.Key, and records its metadata. Callers hold the key
// lock and the shared bucket lock.
//
// The data goes to a new blob, so nothing is overwritten in place: the record
// this write replaces (see prepareKeyForWrite) has its data removed only once
// the new metadata is committed. A crash in between leaves an unreferenced
// file, never a record pointing at the wrong or missing data.
func (fs *FilesystemEngine) commitObject(tmpPath, blobID, blobPath string, info *ObjectInfo) error {
	var obsolete []ObjectInfo
	if info.VersionID == "" {
		var err error
		obsolete, err = fs.metadata.GetObjectVersions(info.Bucket, info.Key)
		if err != nil {
			return err
		}
	}
	replaced, err := fs.prepareKeyForWrite(info.Bucket, info.Key, info.VersionID)
	if err != nil {
		return err
	}
	if err := os.Rename(tmpPath, blobPath); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}
	if err := syncParents(filepath.Dir(blobPath), fs.dataDir); err != nil {
		return err
	}
	info.BlobID = blobID
	if err := fs.metadata.PutObjectMeta(info); err != nil {
		removeObjectFile(blobPath) // never referenced by any record
		return err
	}
	fs.removeData(replaced)
	for i := range obsolete {
		fs.removeData(&obsolete[i])
	}
	return nil
}

// prepareKeyForWrite reconciles what is already stored under key with the
// version about to be committed, and returns the record that version
// replaces, if any. Callers hold the key lock.
//
// An unversioned write replaces the current object and drops the key's
// version history, whose data is removed after PutObjectMeta commits.
// A versioned write first turns an object written before
// versioning was enabled into the "null" version, as S3 does; with
// versioning suspended the write then replaces that null version.
func (fs *FilesystemEngine) prepareKeyForWrite(bucket, key, versionID string) (*ObjectInfo, error) {
	if versionID == "" {
		return fs.storedRecord(bucket, key, "")
	}
	if err := fs.promoteLegacyNullVersion(bucket, key); err != nil {
		return nil, err
	}
	if versionID == "null" {
		return fs.storedRecord(bucket, key, "null")
	}
	return nil, nil
}

// storedRecord returns the record holding data for key at versionID ("" for
// the current, unversioned object), or nil when there is none.
func (fs *FilesystemEngine) storedRecord(bucket, key, versionID string) (*ObjectInfo, error) {
	rec, err := fs.metadata.GetObjectMeta(bucket, key, versionID)
	if errors.Is(err, errObjectMetaNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if rec.IsDeleteMarker || rec.VersionID != versionID {
		return nil, nil
	}
	return rec, nil
}

// promoteLegacyNullVersion makes an object written while the bucket was
// unversioned addressable as version "null". Callers hold the key lock.
//
// Such an object has an empty version ID. The first versioned write or delete
// used to replace its metadata outright, losing the object and orphaning its
// file. Re-recording it as the "null" version lets the existing version logic
// apply unchanged: with versioning Enabled it becomes a noncurrent version;
// with it Suspended the write replaces it, as a Suspended write replaces the
// null version. Blob data does not depend on the version ID; data in the old
// key-path layout is copied into a durable immutable blob.
func (fs *FilesystemEngine) promoteLegacyNullVersion(bucket, key string) error {
	latest, err := fs.metadata.GetObjectMeta(bucket, key, "")
	if errors.Is(err, errObjectMetaNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if latest.VersionID != "" || latest.IsDeleteMarker {
		return nil
	}
	latest.VersionID = "null"
	latest.IsLatest = true
	if latest.BlobID != "" {
		return fs.metadata.PutObjectMetaRaw(latest)
	}

	// Copy legacy bytes to an immutable blob before changing metadata. Moving
	// the old file first would leave its committed record dangling after a crash.
	src, err := fs.objectPathWithVersion(bucket, key, "")
	if err != nil {
		return err
	}
	base := filepath.Join(fs.dataDir, "buckets")
	rel, err := filepath.Rel(base, src)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("invalid legacy object path")
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	input, err := root.Open(rel)
	if err != nil {
		return err
	}
	defer input.Close()
	id, path, err := fs.newBlob(bucket)
	if err != nil {
		return err
	}
	blobRoot, err := os.OpenRoot(fs.dataDir)
	if err != nil {
		return err
	}
	defer blobRoot.Close()
	blobRel := filepath.Join("blobs", bucket, id[:2], id[2:4], id)
	output, err := blobRoot.OpenFile(blobRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if err := blobRoot.Remove(blobRel); err != nil {
				slog.Warn("[Storage] Uncommitted blob cleanup failed", "error", err)
			}
		}
	}()
	_, copyErr := io.Copy(output, input)
	inputCloseErr := input.Close()
	syncErr := output.Sync()
	closeErr := output.Close()
	if err := errors.Join(copyErr, inputCloseErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := syncParents(filepath.Dir(path), fs.dataDir); err != nil {
		return err
	}
	latest.BlobID = id
	if err := fs.metadata.PutObjectMetaRaw(latest); err != nil {
		return err
	}
	committed = true
	// A failed cleanup leaves only an unreferenced legacy copy.
	if err := root.Remove(rel); err != nil {
		slog.Warn("[Storage] Legacy data cleanup failed", "error", err)
	}
	return nil
}

// removeData deletes the file holding a record's data, in whichever layout
// it was written. Delete markers have none.
func (fs *FilesystemEngine) removeData(rec *ObjectInfo) {
	if rec == nil || rec.IsDeleteMarker {
		return
	}
	path, err := fs.dataPath(rec)
	if err != nil {
		slog.Warn("[Storage] Cannot locate object data to remove", "bucket", rec.Bucket, "key", rec.Key, "error", err)
		return
	}
	removeObjectFile(path)
	if rec.BlobID == "" {
		fs.cleanupParentDirs(path, rec.Bucket)
	}
}

// removeObjectFile deletes a stored object's file. A file that is already
// gone is fine; any other failure leaves data on disk that nothing references
// any more, so it is logged rather than silently ignored as it used to be.
func removeObjectFile(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("[Storage] Failed to remove object file; it is now orphaned on disk", "path", path, "error", err)
	}
}

// mirrorAfterVersionDelete brings the replica in line after one specific
// version was removed.
//
// The mirror is a plain, unversioned copy of each key's current state, so a
// DELETE there removes the object outright. Removing a noncurrent version
// leaves the current state unchanged and needs nothing. Removing the current
// version exposes whatever is now latest: re-upload it, or delete the mirror
// copy if nothing readable is left (no versions, or a delete marker on top).
func (fs *FilesystemEngine) mirrorAfterVersionDelete(bucket, key string, wasLatest bool) {
	if !wasLatest {
		return
	}
	next, err := fs.metadata.GetObjectMeta(bucket, key, "")
	if err == nil && !next.IsDeleteMarker {
		fs.MirrorSync(bucket, key, "PUT")
		return
	}
	fs.MirrorSync(bucket, key, "DELETE")
}
