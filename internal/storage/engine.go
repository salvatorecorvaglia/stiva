// Package storage defines the storage engine interface and types for Stiva.
package storage

import (
	"context"
	"encoding/xml"
	"io"
	"net"
	"regexp"
	"strings"
	"time"
)

var bucketNameRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]{1,61}[a-z0-9]$`)

// IsValidBucketName validates S3 bucket naming rules: 3-63 chars, lowercase letters, numbers, hyphens, periods.
// It also ensures it doesn't contain consecutive periods, and isn't formatted as an IP address.
func IsValidBucketName(name string) bool {
	if !bucketNameRegexp.MatchString(name) {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	if net.ParseIP(name) != nil {
		return false
	}
	return true
}

type ssecKeyType struct{}

// SSECContextKey is the context key for SSE-C parameters.
var SSECContextKey = ssecKeyType{}

// SSECParams holds the customer key parameters for SSE-C.
type SSECParams struct {
	Algorithm string
	Key       []byte // 32 bytes for AES-256
	KeyMD5    string // base64-encoded MD5 of the key
}

// CORSRule holds a CORS rule specification.
type CORSRule struct {
	AllowedHeaders []string `json:"allowedHeaders,omitempty" xml:"AllowedHeader"`
	AllowedMethods []string `json:"allowedMethods" xml:"AllowedMethod"`
	AllowedOrigins []string `json:"allowedOrigins" xml:"AllowedOrigin"`
	ExposeHeaders  []string `json:"exposeHeaders,omitempty" xml:"ExposeHeader"`
	MaxAgeSeconds  int      `json:"maxAgeSeconds,omitempty" xml:"MaxAgeSeconds"`
}

// CORSConfiguration holds bucket CORS configuration.
type CORSConfiguration struct {
	CORSRules []CORSRule `json:"corsRules" xml:"CORSRule"`
}

// BucketInfo holds metadata about a storage bucket.
type BucketInfo struct {
	Name         string                  `json:"name"`
	CreationDate time.Time               `json:"creationDate"`
	CORS         *CORSConfiguration      `json:"cors,omitempty"`
	Versioning   string                  `json:"versioning,omitempty"`
	IsPublic     bool                    `json:"isPublic,omitempty"`
	Lifecycle    *LifecycleConfiguration `json:"lifecycle,omitempty"`
	Logging      *BucketLoggingStatus    `json:"logging,omitempty"`
}

// LifecycleConfiguration holds bucket lifecycle configuration.
type LifecycleConfiguration struct {
	XMLName xml.Name        `json:"-" xml:"LifecycleConfiguration"`
	Rules   []LifecycleRule `json:"rules" xml:"Rule"`
}

type LifecycleRule struct {
	ID     string `json:"id" xml:"ID"`
	Status string `json:"status" xml:"Status"` // "Enabled" or "Disabled"
	// Prefix is the deprecated rule-level prefix that predates <Filter>. S3
	// still accepts it and older clients still send it; it used to be dropped
	// on decode, which widened the rule to the whole bucket.
	Prefix                         string                          `json:"prefix,omitempty" xml:"Prefix,omitempty"`
	Filter                         LifecycleFilter                 `json:"filter" xml:"Filter"`
	Expiration                     *LifecycleExpiration            `json:"expiration,omitempty" xml:"Expiration"`
	NoncurrentVersionExpiration    *NoncurrentVersionExpiration    `json:"noncurrentVersionExpiration,omitempty" xml:"NoncurrentVersionExpiration"`
	AbortIncompleteMultipartUpload *AbortIncompleteMultipartUpload `json:"abortIncompleteMultipartUpload,omitempty" xml:"AbortIncompleteMultipartUpload"`
}

// LifecycleFilter mirrors the S3 <Filter> element. Only prefix matching is
// evaluated; the other conditions are decoded solely so that a rule using them
// can be rejected instead of silently applying to every object.
type LifecycleFilter struct {
	Prefix                string                `json:"prefix" xml:"Prefix"`
	Tag                   *LifecycleTag         `json:"tag,omitempty" xml:"Tag,omitempty"`
	And                   *LifecycleAndOperator `json:"and,omitempty" xml:"And,omitempty"`
	ObjectSizeGreaterThan *int64                `json:"objectSizeGreaterThan,omitempty" xml:"ObjectSizeGreaterThan,omitempty"`
	ObjectSizeLessThan    *int64                `json:"objectSizeLessThan,omitempty" xml:"ObjectSizeLessThan,omitempty"`
}

// LifecycleAndOperator mirrors <Filter><And>, which combines several conditions.
type LifecycleAndOperator struct {
	Prefix                string         `json:"prefix,omitempty" xml:"Prefix,omitempty"`
	Tags                  []LifecycleTag `json:"tags,omitempty" xml:"Tag,omitempty"`
	ObjectSizeGreaterThan *int64         `json:"objectSizeGreaterThan,omitempty" xml:"ObjectSizeGreaterThan,omitempty"`
	ObjectSizeLessThan    *int64         `json:"objectSizeLessThan,omitempty" xml:"ObjectSizeLessThan,omitempty"`
}

// LifecycleTag is an object tag condition in a lifecycle filter.
type LifecycleTag struct {
	Key   string `json:"key" xml:"Key"`
	Value string `json:"value" xml:"Value"`
}

type LifecycleExpiration struct {
	Days int `json:"days,omitempty" xml:"Days"`
}

type NoncurrentVersionExpiration struct {
	NoncurrentDays int `json:"noncurrentDays,omitempty" xml:"NoncurrentDays"`
}

type AbortIncompleteMultipartUpload struct {
	DaysAfterInitiation int `json:"daysAfterInitiation,omitempty" xml:"DaysAfterInitiation"`
}

// BucketLoggingStatus represents the bucket logging configuration.
type BucketLoggingStatus struct {
	XMLName        xml.Name        `json:"-" xml:"BucketLoggingStatus"`
	LoggingEnabled *LoggingEnabled `json:"loggingEnabled,omitempty" xml:"LoggingEnabled"`
}

type LoggingEnabled struct {
	TargetBucket string `json:"targetBucket" xml:"TargetBucket"`
	TargetPrefix string `json:"targetPrefix" xml:"TargetPrefix"`
}

// ObjectInfo holds metadata about a stored object.
type ObjectInfo struct {
	Bucket               string    `json:"bucket"`
	Key                  string    `json:"key"`
	Size                 int64     `json:"size"`
	ETag                 string    `json:"etag"`
	ContentType          string    `json:"contentType"`
	LastModified         time.Time `json:"lastModified"`
	VersionID            string    `json:"versionId,omitempty"`
	IsLatest             bool      `json:"isLatest,omitempty"`
	IsDeleteMarker       bool      `json:"isDeleteMarker,omitempty"`
	SSECustomerAlgorithm string    `json:"sseCustomerAlgorithm,omitempty"`
	SSECustomerKeyMD5    string    `json:"sseCustomerKeyMD5,omitempty"`
	Compressed           bool      `json:"compressed,omitempty"`
	// BlobID names the file holding this version's data (see blobPath).
	// Empty for data written before the blob layout, which is still found
	// at its old key-derived path.
	BlobID string `json:"blobId,omitempty"`
}

// CopySource identifies the source of a CopyObject operation.
type CopySource struct {
	Bucket string
	Key    string
	// VersionID optionally selects a specific source version.
	VersionID string
	// SSEC carries the customer key for an SSE-C encrypted source, if any.
	SSEC *SSECParams
}

// ListObjectsInput holds parameters for listing objects in a bucket.
type ListObjectsInput struct {
	Bucket            string
	Prefix            string
	Delimiter         string
	MaxKeys           int
	ContinuationToken string
	StartAfter        string
}

// ListObjectsOutput holds the result of listing objects in a bucket.
type ListObjectsOutput struct {
	Objects               []ObjectInfo
	CommonPrefixes        []string
	IsTruncated           bool
	NextContinuationToken string
	KeyCount              int
}

// MultipartUploadInfo holds metadata about a multipart upload.
type MultipartUploadInfo struct {
	UploadID string
	Bucket   string
	Key      string
	Created  time.Time
}

// PartInfo holds metadata about a single uploaded part.
type PartInfo struct {
	// BlobID selects immutable part data. Empty reads the legacy numbered file.
	BlobID     string `json:"blobId,omitempty"`
	PartNumber int
	ETag       string
	Size       int64
}

// CompletePart represents a part in a CompleteMultipartUpload request.
type CompletePart struct {
	PartNumber int
	ETag       string
}

// BucketStore creates, removes and enumerates buckets.
type BucketStore interface {
	CreateBucket(name string) error
	DeleteBucket(name string) error
	BucketExists(name string) (bool, error)
	ListBuckets() ([]BucketInfo, error)
}

// BucketConfigStore reads and writes per-bucket settings.
type BucketConfigStore interface {
	PutBucketCORS(bucket string, cors *CORSConfiguration) error
	GetBucketCORS(bucket string) (*CORSConfiguration, error)
	DeleteBucketCORS(bucket string) error
	SetBucketVersioning(bucket, status string) error
	GetBucketVersioning(bucket string) (string, error)
	SetBucketPublic(bucket string, public bool) error
	IsBucketPublic(bucket string) (bool, error)
	PutBucketLifecycle(bucket string, lc *LifecycleConfiguration) error
	GetBucketLifecycle(bucket string) (*LifecycleConfiguration, error)
	DeleteBucketLifecycle(bucket string) error
	PutBucketLogging(bucket string, logging *BucketLoggingStatus) error
	GetBucketLogging(bucket string) (*BucketLoggingStatus, error)
}

// ObjectStore reads, writes and lists objects.
type ObjectStore interface {
	PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error)
	GetObject(ctx context.Context, bucket, key, versionID string) (io.ReadCloser, *ObjectInfo, error)
	HeadObject(ctx context.Context, bucket, key, versionID string) (*ObjectInfo, error)
	DeleteObject(bucket, key, versionID string) (isDeleteMarker bool, delVersionID string, err error)
	CopyObject(ctx context.Context, src CopySource, dstBucket, dstKey string) (*ObjectInfo, error)
	ListObjects(input *ListObjectsInput) (*ListObjectsOutput, error)
	ListObjectVersions(input *ListVersionsInput) (*ListVersionsOutput, error)
	CountObjects(bucket string) (int, error)
}

// MultipartStore runs multipart uploads.
type MultipartStore interface {
	// CreateMultipartUpload fixes the upload's SSE-C mode from the key in ctx,
	// if any; every part and the completion must then present the same key.
	CreateMultipartUpload(ctx context.Context, bucket, key, contentType string) (*MultipartUploadInfo, error)
	UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int, reader io.Reader, size int64) (*PartInfo, error)
	// CompleteMultipartUpload requires the SSE-C customer key in ctx when the
	// upload was created with SSE-C, because the key is never persisted.
	CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []CompletePart) (*ObjectInfo, error)
	AbortMultipartUpload(bucket, key, uploadID string) error
	ListMultipartUploads(bucket, prefix, keyMarker, uploadIDMarker string, maxUploads int) ([]MultipartUploadInfo, bool, error)
	ListParts(bucket, key, uploadID string) ([]PartInfo, error)
}

// Maintainer covers what only the server itself runs: background sweeps,
// system settings, and shutdown.
type Maintainer interface {
	// The cleanup sweeps stop between items once ctx is cancelled.
	CleanExpiredObjects(ctx context.Context) error
	CleanExpiredMultipartUploads(ctx context.Context, cutoff time.Duration) error
	GetSystemValue(key string) (string, error)
	PutSystemValue(key, val string) error
	// Shutdown drains background work within ctx's deadline, then releases
	// the engine. Close is Shutdown with no deadline.
	Shutdown(ctx context.Context) error
	Close() error
}

// DataStore is everything a request handler needs: the S3 API and the console
// depend on this rather than on the full Engine, so they cannot reach the
// maintenance operations.
type DataStore interface {
	BucketStore
	BucketConfigStore
	ObjectStore
	MultipartStore
}

// Engine is the interface that all storage backends must implement.
type Engine interface {
	DataStore
	Maintainer
}

// FilesystemEngine must implement Engine; this fails the build if it drifts.
var _ Engine = (*FilesystemEngine)(nil)
