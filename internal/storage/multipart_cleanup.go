package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

type expiredUpload struct{ bucket, key, uploadID string }

// Collect only a bounded batch, closing the metadata transaction before any
// abort re-enters the store. Corruption and read errors must stop deletion.
func (fs *FilesystemEngine) expiredMultipartUploads(ctx context.Context, bucket, prefix string, now time.Time, age time.Duration, limit int) ([]expiredUpload, error) {
	var uploads []expiredUpload
	err := fs.metadata.IterateMultipartMetas(bucket, func(meta *MultipartMeta) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(uploads) >= limit {
			return errExpiryBatchFull
		}
		if strings.HasPrefix(meta.Key, prefix) && now.Sub(meta.Created) > age {
			uploads = append(uploads, expiredUpload{bucket: bucket, key: meta.Key, uploadID: meta.UploadID})
		}
		return nil
	})
	if err != nil && !errors.Is(err, errExpiryBatchFull) {
		return nil, fmt.Errorf("scan expired multipart uploads in %s: %w", bucket, err)
	}
	return uploads, nil
}

func (fs *FilesystemEngine) abortExpiredUploads(ctx context.Context, uploads []expiredUpload) error {
	var failures []error
	for _, upload := range uploads {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		err := fs.AbortMultipartUpload(upload.bucket, upload.key, upload.uploadID)
		var s3err *S3Error
		if errors.As(err, &s3err) && s3err.Code == "NoSuchUpload" {
			continue
		} // another caller already finished it
		if err != nil {
			failures = append(failures, fmt.Errorf("abort multipart %s/%s (%s): %w", upload.bucket, upload.key, upload.uploadID, err))
			continue
		}
		slog.Info("[Storage] Expired multipart upload removed", "bucket", upload.bucket, "key", upload.key, "uploadID", upload.uploadID)
	}
	return errors.Join(failures...)
}

// CleanExpiredMultipartUploads reports scan and abort failures, retaining
// failed upload metadata so later sweeps can retry. Each sweep is bounded.
func (fs *FilesystemEngine) CleanExpiredMultipartUploads(ctx context.Context, cutoff time.Duration) error {
	buckets, err := fs.metadata.ListBuckets()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var uploads []expiredUpload
	for _, bucket := range buckets {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := fs.expiredMultipartUploads(ctx, bucket.Name, "", now, cutoff, maxExpiryBatch-len(uploads))
		if err != nil {
			return err
		}
		uploads = append(uploads, batch...)
		if len(uploads) >= maxExpiryBatch {
			break
		}
	}
	return fs.abortExpiredUploads(ctx, uploads)
}
