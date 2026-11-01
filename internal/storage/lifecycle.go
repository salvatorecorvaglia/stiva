package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// maxLifecycleRules matches the S3 limit on rules per configuration.
const maxLifecycleRules = 1000

// EffectivePrefix returns the key prefix a rule applies to, whichever of the
// three places S3 allows it to be written in: the legacy rule-level <Prefix>,
// <Filter><Prefix>, or <Filter><And><Prefix>. Validate guarantees at most one
// of them is set.
func (r LifecycleRule) EffectivePrefix() string {
	switch {
	case r.Prefix != "":
		return r.Prefix
	case r.Filter.And != nil:
		return r.Filter.And.Prefix
	default:
		return r.Filter.Prefix
	}
}

// Validate rejects configurations Stiva would otherwise apply more broadly
// than the caller intended. Any filter condition the sweep does not evaluate
// (tags, object size) is refused rather than ignored: ignoring a condition
// turns "expire these objects" into "expire all objects under this prefix".
func (lc *LifecycleConfiguration) Validate() error {
	if lc == nil || len(lc.Rules) == 0 {
		return &S3Error{Code: "MalformedXML", Message: "The lifecycle configuration must contain at least one rule."}
	}
	if len(lc.Rules) > maxLifecycleRules {
		return &S3Error{Code: "MalformedXML", Message: fmt.Sprintf("The lifecycle configuration cannot contain more than %d rules.", maxLifecycleRules)}
	}
	for i := range lc.Rules {
		if err := lc.Rules[i].validate(); err != nil {
			return err
		}
	}
	return nil
}

func (r LifecycleRule) validate() error {
	if !strings.EqualFold(r.Status, "Enabled") && !strings.EqualFold(r.Status, "Disabled") {
		return &S3Error{Code: "MalformedXML", Message: fmt.Sprintf("Rule %q: Status must be Enabled or Disabled.", r.ID)}
	}

	f := r.Filter
	if f.Tag != nil || f.ObjectSizeGreaterThan != nil || f.ObjectSizeLessThan != nil {
		return &S3Error{Code: "NotImplemented", Message: fmt.Sprintf("Rule %q: only prefix filters are supported by this server.", r.ID)}
	}
	if f.And != nil {
		if len(f.And.Tags) > 0 || f.And.ObjectSizeGreaterThan != nil || f.And.ObjectSizeLessThan != nil {
			return &S3Error{Code: "NotImplemented", Message: fmt.Sprintf("Rule %q: only prefix filters are supported by this server.", r.ID)}
		}
		if f.Prefix != "" {
			return &S3Error{Code: "MalformedXML", Message: fmt.Sprintf("Rule %q: Filter cannot combine Prefix with And.", r.ID)}
		}
	}
	if r.Prefix != "" && (f.Prefix != "" || f.And != nil) {
		return &S3Error{Code: "MalformedXML", Message: fmt.Sprintf("Rule %q: Prefix and Filter cannot both be specified.", r.ID)}
	}

	if r.Expiration != nil && r.Expiration.Days < 0 {
		return &S3Error{Code: "InvalidArgument", Message: fmt.Sprintf("Rule %q: Expiration Days must be a positive integer.", r.ID)}
	}
	if r.NoncurrentVersionExpiration != nil && r.NoncurrentVersionExpiration.NoncurrentDays < 0 {
		return &S3Error{Code: "InvalidArgument", Message: fmt.Sprintf("Rule %q: NoncurrentDays must be a positive integer.", r.ID)}
	}
	if r.AbortIncompleteMultipartUpload != nil && r.AbortIncompleteMultipartUpload.DaysAfterInitiation < 0 {
		return &S3Error{Code: "InvalidArgument", Message: fmt.Sprintf("Rule %q: DaysAfterInitiation must be a positive integer.", r.ID)}
	}
	return nil
}

func (fs *FilesystemEngine) PutBucketLifecycle(bucket string, lc *LifecycleConfiguration) error {
	if err := fs.validateBucketName(bucket); err != nil {
		return err
	}
	if err := lc.Validate(); err != nil {
		return err
	}
	if err := fs.bucketMustExist(bucket); err != nil {
		return err
	}
	return fs.metadata.PutBucketLifecycle(bucket, lc)
}

func (fs *FilesystemEngine) GetBucketLifecycle(bucket string) (*LifecycleConfiguration, error) {
	if err := fs.requireBucket(bucket); err != nil {
		return nil, err
	}
	lc, err := fs.metadata.GetBucketLifecycle(bucket)
	if err != nil {
		return nil, err
	}
	if lc == nil {
		return nil, &S3Error{Code: "NoSuchLifecycleConfiguration", Message: "The lifecycle configuration does not exist"}
	}
	return lc, nil
}

func (fs *FilesystemEngine) DeleteBucketLifecycle(bucket string) error {
	if err := fs.requireBucket(bucket); err != nil {
		return err
	}
	return fs.metadata.DeleteBucketLifecycle(bucket)
}

func (fs *FilesystemEngine) PutBucketLogging(bucket string, logging *BucketLoggingStatus) error {
	if err := fs.requireBucket(bucket); err != nil {
		return err
	}
	return fs.metadata.PutBucketLogging(bucket, logging)
}

func (fs *FilesystemEngine) GetBucketLogging(bucket string) (*BucketLoggingStatus, error) {
	if err := fs.requireBucket(bucket); err != nil {
		return nil, err
	}
	return fs.metadata.GetBucketLogging(bucket)
}

// maxExpiryBatch bounds how many expiring records are held in memory per sweep
// pass. The cleanup workers used to accumulate every match across every bucket
// before acting, which is unbounded on a large installation. Remaining items are
// simply picked up by the next hourly pass.
const maxExpiryBatch = 10000

// errExpiryBatchFull stops an iteration early once the batch cap is reached.
var errExpiryBatchFull = errors.New("expiry batch full")

// CleanExpiredObjects applies every bucket's lifecycle rules. It returns
// ctx's error, leaving the rest for the next sweep, once ctx is cancelled.
func (fs *FilesystemEngine) CleanExpiredObjects(ctx context.Context) error {
	buckets, err := fs.ListBuckets()
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, bInfo := range buckets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if bInfo.Lifecycle == nil || len(bInfo.Lifecycle.Rules) == 0 {
			continue
		}

		for _, rule := range bInfo.Lifecycle.Rules {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !strings.EqualFold(rule.Status, "Enabled") {
				continue
			}
			// A configuration stored before validation existed may still
			// carry a condition the sweep cannot evaluate. Skipping the rule
			// is the only safe reading: ignoring the condition would widen it.
			if err := rule.validate(); err != nil {
				slog.Warn("[Storage] Skipping invalid lifecycle rule", "bucket", bInfo.Name, "rule", rule.ID, "error", err)
				continue
			}
			prefix := rule.EffectivePrefix()

			// 1. Current Version Expiration
			if rule.Expiration != nil && rule.Expiration.Days > 0 {
				cutoff := time.Duration(rule.Expiration.Days) * 24 * time.Hour
				var expiredList []struct {
					key          string
					versionID    string
					lastModified time.Time
				}
				scanErr := fs.metadata.IterateObjectMetas(bInfo.Name, func(m *ObjectInfo) error {
					if len(expiredList) >= maxExpiryBatch {
						return errExpiryBatchFull
					}
					if !strings.HasPrefix(m.Key, prefix) {
						return nil
					}
					// Check if expired
					if now.Sub(m.LastModified) > cutoff {
						expiredList = append(expiredList, struct {
							key          string
							versionID    string
							lastModified time.Time
						}{key: m.Key, versionID: m.VersionID, lastModified: m.LastModified})
					}
					return nil
				})
				if scanErr != nil && !errors.Is(scanErr, errExpiryBatchFull) {
					return scanErr
				}

				for _, exp := range expiredList {
					if err := ctx.Err(); err != nil {
						return err
					}
					if err := fs.expireCurrentVersion(bInfo.Name, exp.key, exp.versionID, exp.lastModified); err != nil {
						return err
					}

				}
			}

			// 2. Noncurrent Version Expiration
			if rule.NoncurrentVersionExpiration != nil && rule.NoncurrentVersionExpiration.NoncurrentDays > 0 {
				cutoff := time.Duration(rule.NoncurrentVersionExpiration.NoncurrentDays) * 24 * time.Hour
				var expiredNoncurrentList []struct {
					key       string
					versionID string
				}
				scanErr := fs.metadata.IterateObjectVersions(bInfo.Name, func(v *ObjectInfo) error {
					if len(expiredNoncurrentList) >= maxExpiryBatch {
						return errExpiryBatchFull
					}
					if v.IsLatest {
						return nil // Only noncurrent versions
					}
					if !strings.HasPrefix(v.Key, prefix) {
						return nil
					}
					if v.NoncurrentSince.IsZero() || now.Sub(v.NoncurrentSince) > cutoff {
						expiredNoncurrentList = append(expiredNoncurrentList, struct {
							key       string
							versionID string
						}{key: v.Key, versionID: v.VersionID})
					}
					return nil
				})
				if scanErr != nil && !errors.Is(scanErr, errExpiryBatchFull) {
					return scanErr
				}

				for _, exp := range expiredNoncurrentList {
					if err := ctx.Err(); err != nil {
						return err
					}
					if err := fs.expireNoncurrentVersion(bInfo.Name, exp.key, exp.versionID, now, cutoff); err != nil {
						return err
					}

				}
			}

			// 3. Abort Incomplete Multipart Upload
			if rule.AbortIncompleteMultipartUpload != nil && rule.AbortIncompleteMultipartUpload.DaysAfterInitiation > 0 {
				cutoff := time.Duration(rule.AbortIncompleteMultipartUpload.DaysAfterInitiation) * 24 * time.Hour
				uploads, err := fs.expiredMultipartUploads(ctx, bInfo.Name, prefix, now, cutoff, maxExpiryBatch)
				if err != nil {
					return err
				}
				if err := fs.abortExpiredUploads(ctx, uploads); err != nil {
					return err
				}

			}
		}
	}
	return nil
}
