package storage

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

const (
	errInvalidBucketTraversal = "invalid bucket name: path traversal detected"
	errInvalidKeyTraversal    = "invalid object key: path traversal detected"
	errInvalidUploadID        = "invalid upload ID: path traversal detected"
	errBucketNotFound         = "The specified bucket does not exist"
	errKeyNotFound            = "The specified key does not exist."
	errUploadNotFound         = "The specified multipart upload does not exist."
	errFailedGenerateIV       = "failed to generate random IV: %w"
	errFailedCreateAES        = "failed to create AES cipher: %w"
)

var (
	gzipWriterPool = sync.Pool{
		New: func() interface{} {
			w, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
			return w
		},
	}
	gzipReaderPool sync.Pool
)

func getGzipReader(r io.Reader) (*gzip.Reader, error) {
	if v := gzipReaderPool.Get(); v != nil {
		gr := v.(*gzip.Reader)
		if err := gr.Reset(r); err != nil {
			return nil, err
		}
		return gr, nil
	}
	return gzip.NewReader(r)
}

func putGzipReader(gr *gzip.Reader) {
	gzipReaderPool.Put(gr)
}

type pooledGzipReader struct {
	*gzip.Reader
	closeOnce sync.Once
	closeErr  error
}

// Close returns the reader to the pool exactly once. It used to put it back on
// every call, and object readers do get closed twice (net/http closes a
// request body it was given, and the owner closes it too), which let two later
// callers decompress through the same shared gzip.Reader. The Once also makes
// it safe for those two closes to run concurrently.
func (p *pooledGzipReader) Close() error {
	p.closeOnce.Do(func() {
		p.closeErr = p.Reader.Close()
		putGzipReader(p.Reader)
	})
	return p.closeErr
}

// FilesystemEngine implements the Engine interface using the local filesystem.
// Objects are stored as files under <dataDir>/buckets/<bucket>/<key>.
// Metadata is persisted in a bbolt database.
type FilesystemEngine struct {
	dataDir  string
	metadata *MetadataStore
	mu       sync.Mutex
	locks    map[string]*uploadLock
	// bucketLocks backs lockBucket; guarded by mu like locks.
	bucketLocks map[string]*bucketRWLock

	// disableMinPartSize turns off the 5MB minimum part size on
	// CompleteMultipartUpload. It is threaded in from configuration rather
	// than read from the environment mid-request.
	disableMinPartSize bool

	// maxObjectSize caps the bytes PutObject/UploadPart will write for a
	// single object/part, independent of the SigV4 layer's own payload-size
	// check (which a client sending UNSIGNED-PAYLOAD bypasses entirely).
	// Zero disables the cap.
	maxObjectSize int64

	// Sync replication queue
	syncConfig         *SyncConfig
	syncQueue          chan syncTask
	syncOnce           sync.Once
	syncWG             sync.WaitGroup
	isSyncShuttingDown int32
	syncMu             sync.Mutex
	syncClient         *http.Client

	// Webhook queue
	webhookURL            string
	webhookSecret         string
	webhookQueue          chan webhookTask
	webhookOnce           sync.Once
	webhookWG             sync.WaitGroup
	isWebhookShuttingDown int32
	webhookMu             sync.Mutex

	// metrics receives this engine's counters; see Metrics.
	metrics *MetricsTracker

	// dispatchCtx scopes every replication and webhook delivery, retry wait
	// and queued event. Shutdown cancels it once its deadline passes, which
	// aborts in-flight requests and abandons what is left instead of draining
	// it without limit. Created on first use (see dispatchContext).
	dispatchOnce   sync.Once
	dispatchCtx    context.Context
	dispatchCancel context.CancelFunc
}

type uploadLock struct {
	sync.Mutex
	refCount int
}

// NewFilesystemEngine creates a new filesystem-backed storage engine.
func NewFilesystemEngine(dataDir string, syncCfg *SyncConfig, webhookURL string) (*FilesystemEngine, error) {
	bucketsDir := filepath.Join(dataDir, "buckets")
	if err := os.MkdirAll(bucketsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create buckets directory: %w", err)
	}

	multipartDir := filepath.Join(dataDir, "multipart")
	if err := os.MkdirAll(multipartDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create multipart directory: %w", err)
	}

	meta, err := NewMetadataStore(dataDir)
	if err != nil {
		return nil, err
	}

	cleanupOrphanedTempFiles(dataDir)
	seedActiveMultipartGauge(meta, GlobalMetrics)

	return &FilesystemEngine{
		metrics:    GlobalMetrics,
		dataDir:    dataDir,
		metadata:   meta,
		locks:      make(map[string]*uploadLock),
		syncConfig: syncCfg,
		syncClient: &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				MaxIdleConnsPerHost:   10,
			},
		},
		webhookURL: webhookURL,
	}, nil
}

// Metrics returns the tracker this engine records into: GlobalMetrics unless
// SetMetrics chose another.
func (fs *FilesystemEngine) Metrics() *MetricsTracker {
	if fs.metrics == nil {
		return GlobalMetrics
	}
	return fs.metrics
}

// SetMetrics makes the engine record into m instead of GlobalMetrics, seeding
// m's in-progress multipart gauge from the uploads already stored. Like the
// other setters it must be called before the engine serves traffic.
func (fs *FilesystemEngine) SetMetrics(m *MetricsTracker) {
	fs.metrics = m
	seedActiveMultipartGauge(fs.metadata, m)
}

// SetWebhookSecret sets the HMAC secret used to sign outgoing webhook
// payloads, so receivers can authenticate them. Without it, any host that
// learns the webhook URL can forge events. Must be called before the first
// event is triggered; the dispatcher reads it per-delivery, not once at
// startup, so it isn't safe to change concurrently with event delivery.
func (fs *FilesystemEngine) SetWebhookSecret(secret string) {
	fs.webhookSecret = secret
}

// SetMaxObjectSize caps the bytes PutObject/UploadPart will accept for a
// single object/part. Zero (the default) disables the cap.
func (fs *FilesystemEngine) SetMaxObjectSize(n int64) {
	fs.maxObjectSize = n
}

// SetDisableMinPartSize turns off the S3 rule requiring every multipart part
// except the last to be at least 5MB. Like the other setters here it must be
// called before the engine serves traffic.
func (fs *FilesystemEngine) SetDisableMinPartSize(v bool) {
	fs.disableMinPartSize = v
}

// Close stops the replication and webhook workers, waiting for everything
// already queued to be delivered, then closes the metadata store. It waits as
// long as that takes; Shutdown bounds it.
func (fs *FilesystemEngine) Close() error {
	return fs.Shutdown(context.Background())
}

// Shutdown stops accepting replication and webhook events, delivers what is
// already queued, and closes the metadata store.
//
// The drain used to be unbounded: each queue holds up to 5,000 events and a
// replication transfer may run for minutes, so a slow or unreachable target
// held the process long past any shutdown timeout. When ctx expires first,
// in-flight deliveries are cancelled, the remaining events are abandoned and
// counted in the dropped-event metrics, and the error reports the deadline.
func (fs *FilesystemEngine) Shutdown(ctx context.Context) error {
	fs.beginSyncStop()
	fs.beginWebhookStop()

	drained := make(chan struct{})
	go func() {
		fs.syncWG.Wait()
		fs.webhookWG.Wait()
		close(drained)
	}()

	var drainErr error
	select {
	case <-drained:
	case <-ctx.Done():
		drainErr = fmt.Errorf("event queues not drained before the shutdown deadline; undelivered events were counted as dropped: %w", ctx.Err())
		slog.Warn("[Storage] Shutdown deadline reached, abandoning undelivered replication and webhook events")
		fs.dispatchContext() // make sure there is a cancel func to call
		fs.dispatchCancel()
		<-drained // cancellation makes every worker and retry return promptly
	}

	return errors.Join(drainErr, fs.metadata.Close())
}

// dispatchContext returns the context governing event delivery.
func (fs *FilesystemEngine) dispatchContext() context.Context {
	fs.dispatchOnce.Do(func() {
		fs.dispatchCtx, fs.dispatchCancel = context.WithCancel(context.Background())
	})
	return fs.dispatchCtx
}

// sleepCtx waits for d, reporting false if ctx was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (fs *FilesystemEngine) GetSystemValue(key string) (string, error) {
	return fs.metadata.GetSystemValue(key)
}

func (fs *FilesystemEngine) PutSystemValue(key, val string) error {
	return fs.metadata.PutSystemValue(key, val)
}

// CreateBucket creates a new storage bucket.
func (fs *FilesystemEngine) CreateBucket(name string) error {
	if err := fs.validateBucketName(name); err != nil {
		return err
	}

	// Held across the check and the create, which otherwise let concurrent
	// creates of one name all report success.
	unlock := fs.lockBucket(name, true)
	defer unlock()

	exists, err := fs.metadata.BucketExists(name)
	if err != nil {
		return err
	}
	if exists {
		return &S3Error{Code: "BucketAlreadyOwnedByYou", Message: "Your previous request to create the named bucket succeeded and you already own it."}
	}

	if err := os.MkdirAll(fs.bucketPath(name), 0755); err != nil {
		return fmt.Errorf("failed to create bucket directory: %w", err)
	}

	return fs.metadata.PutBucket(&BucketInfo{
		Name:         name,
		CreationDate: time.Now().UTC(),
	})
}

// DeleteBucket deletes a bucket if it's empty.
func (fs *FilesystemEngine) DeleteBucket(name string) error {
	if err := fs.validateBucketName(name); err != nil {
		return err
	}

	// Exclusive across the emptiness check and the removal: every object and
	// multipart commit holds this lock shared (see lockBucketForCommit), so no
	// write can land between "it's empty" and "it's gone".
	unlock := fs.lockBucket(name, true)
	defer unlock()

	if err := fs.bucketMustExist(name); err != nil {
		return err
	}

	// Check if bucket has objects, delete markers, or active multipart uploads
	empty, err := fs.metadata.IsBucketEmpty(name)
	if err != nil {
		return err
	}
	if !empty {
		return &S3Error{Code: "BucketNotEmpty", Message: "The bucket you tried to delete is not empty"}
	}

	if err := os.RemoveAll(fs.bucketPath(name)); err != nil {
		return fmt.Errorf("failed to remove bucket directory: %w", err)
	}
	if err := os.RemoveAll(fs.blobDir(name)); err != nil {
		return fmt.Errorf("failed to remove bucket data directory: %w", err)
	}

	return fs.metadata.DeleteBucket(name)
}

// BucketExists checks if a bucket exists.
func (fs *FilesystemEngine) BucketExists(name string) (bool, error) {
	if err := fs.validateBucketName(name); err != nil {
		return false, err
	}
	return fs.metadata.BucketExists(name)
}

// ListBuckets returns all buckets.
func (fs *FilesystemEngine) ListBuckets() ([]BucketInfo, error) {
	return fs.metadata.ListBuckets()
}

// PutBucketCORS sets CORS configuration for a bucket.
func (fs *FilesystemEngine) PutBucketCORS(bucket string, cors *CORSConfiguration) error {
	if err := fs.requireBucket(bucket); err != nil {
		return err
	}
	return fs.metadata.PutBucketCORS(bucket, cors)
}

// GetBucketCORS gets CORS configuration for a bucket.
func (fs *FilesystemEngine) GetBucketCORS(bucket string) (*CORSConfiguration, error) {
	if err := fs.requireBucket(bucket); err != nil {
		return nil, err
	}
	return fs.metadata.GetBucketCORS(bucket)
}

// DeleteBucketCORS deletes CORS configuration for a bucket.
func (fs *FilesystemEngine) DeleteBucketCORS(bucket string) error {
	if err := fs.requireBucket(bucket); err != nil {
		return err
	}
	return fs.metadata.DeleteBucketCORS(bucket)
}

// CountObjects returns the number of objects in a bucket without loading metadata.
func (fs *FilesystemEngine) CountObjects(bucket string) (int, error) {
	if err := fs.requireBucket(bucket); err != nil {
		return 0, err
	}
	return fs.metadata.CountObjectMetas(bucket)
}

// PutObject stores an object, streaming data directly to disk.
func (fs *FilesystemEngine) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, contentType string) (*ObjectInfo, error) {
	if hasPathTraversal(key) || hasPathTraversal(bucket) {
		return nil, &S3Error{Code: "InvalidArgument", Message: errInvalidKeyTraversal}
	}
	if err := fs.validateBucketName(bucket); err != nil {
		return nil, err
	}
	if err := validateNewKey(key); err != nil {
		return nil, err
	}
	// A declared Content-Length over the cap is rejected immediately; an
	// unknown/chunked size (-1) still passes here but is bounded below by
	// wrapping reader in a LimitReader, so this applies regardless of
	// signing mode — including UNSIGNED-PAYLOAD, which bypasses the SigV4
	// layer's own payload size check entirely.
	if fs.maxObjectSize > 0 && size > fs.maxObjectSize {
		return nil, &S3Error{Code: "EntityTooLarge", Message: "Your proposed upload exceeds the maximum allowed object size."}
	}

	if err := fs.bucketMustExist(bucket); err != nil {
		return nil, err
	}

	versionStatus, err := fs.metadata.GetBucketVersioning(bucket)
	if err != nil {
		versionStatus = ""
	}

	var versionID string
	switch versionStatus {
	case "Enabled":
		versionID = uuid.New().String()
	case "Suspended":
		versionID = "null"
	}

	if _, err := fs.objectPathWithVersion(bucket, key, versionID); err != nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}
	blobID, blobPath, err := fs.newBlob(bucket)
	if err != nil {
		return nil, err
	}

	// Write to a temp file first, then rename for atomicity
	tmpFile, err := os.CreateTemp(filepath.Dir(blobPath), ".stiva-tmp-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		tmpFile.Close()
		os.Remove(tmpPath) // Clean up on failure; no-op if already renamed
	}()

	// Extract SSECParams from context
	var ssecParams *SSECParams
	if ctx != nil {
		if params, ok := ctx.Value(SSECContextKey).(*SSECParams); ok && params != nil {
			ssecParams = params
		}
	}

	var iv []byte
	if ssecParams != nil {
		// Generate random IV
		iv = make([]byte, 16)
		if _, err := io.ReadFull(rand.Reader, iv); err != nil {
			return nil, fmt.Errorf(errFailedGenerateIV, err)
		}
		if _, err := tmpFile.Write(iv); err != nil {
			return nil, fmt.Errorf("failed to write IV to file: %w", err)
		}
	}

	bufWriter := bufio.NewWriterSize(tmpFile, 64*1024)
	var out io.Writer = bufWriter

	if ssecParams != nil {
		block, err := aes.NewCipher(ssecParams.Key)
		if err != nil {
			return nil, fmt.Errorf(errFailedCreateAES, err)
		}
		stream := cipher.NewCTR(block, iv)
		out = &cipher.StreamWriter{S: stream, W: out}
	}

	var gzipWriter *gzip.Writer
	defer func() {
		if gzipWriter != nil {
			gzipWriterPool.Put(gzipWriter)
		}
	}()

	compressed := shouldCompress(contentType, size)

	if compressed {
		gw := gzipWriterPool.Get().(*gzip.Writer)
		gw.Reset(out)
		gzipWriter = gw
		out = gw
	}

	// Stream data to disk while computing MD5
	hash := md5.New()
	capped := reader
	if fs.maxObjectSize > 0 {
		capped = io.LimitReader(reader, fs.maxObjectSize+1)
	}
	written, err := io.Copy(io.MultiWriter(out, hash), capped)
	if err != nil {
		return nil, fmt.Errorf("failed to write object data: %w", err)
	}
	if fs.maxObjectSize > 0 && written > fs.maxObjectSize {
		return nil, &S3Error{Code: "EntityTooLarge", Message: "Your proposed upload exceeds the maximum allowed object size."}
	}

	if gzipWriter != nil {
		if err := gzipWriter.Close(); err != nil {
			return nil, fmt.Errorf("failed to close gzip writer: %w", err)
		}
		gzipWriterPool.Put(gzipWriter)
		gzipWriter = nil
	}

	if err := bufWriter.Flush(); err != nil {
		return nil, fmt.Errorf("failed to flush buffer: %w", err)
	}

	// Validate size mismatch (truncated uploads)
	if size >= 0 && written != size {
		return nil, &S3Error{Code: "BadRequest", Message: fmt.Sprintf("Size mismatch: expected %d bytes, wrote %d bytes", size, written)}
	}

	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp file: %w", err)
	}

	// Serializes the rename-then-metadata-write sequence below for this exact
	// key, so two racing PUTs to the same unversioned key can't leave the
	// on-disk bytes from one request paired with the ETag/size metadata from
	// the other.
	unlockObj := fs.lockObjectKey(bucket, key)
	defer unlockObj()

	unlockBucket, err := fs.lockBucketForCommit(bucket)
	if err != nil {
		return nil, err
	}
	defer unlockBucket()

	etag := hex.EncodeToString(hash.Sum(nil))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	info := &ObjectInfo{
		Bucket:       bucket,
		Key:          key,
		Size:         written,
		ETag:         etag,
		ContentType:  contentType,
		LastModified: time.Now().UTC(),
		VersionID:    versionID,
		Compressed:   compressed,
	}

	if ssecParams != nil {
		info.SSECustomerAlgorithm = ssecParams.Algorithm
		info.SSECustomerKeyMD5 = ssecParams.KeyMD5
	}

	if err := fs.commitObject(tmpPath, blobID, blobPath, info); err != nil {
		return nil, err
	}

	fs.Metrics().AddUploaded(written)

	fs.triggerWebhook("ObjectCreated:Put", info)

	fs.MirrorSync(bucket, key, "PUT")

	return info, nil
}

// GetObject retrieves an object's data and metadata.
func (fs *FilesystemEngine) GetObject(ctx context.Context, bucket, key, versionID string) (io.ReadCloser, *ObjectInfo, error) {
	if err := fs.validateBucketName(bucket); err != nil {
		return nil, nil, err
	}

	info, err := fs.HeadObject(ctx, bucket, key, versionID)
	if err != nil {
		return nil, info, err
	}

	objPath, err := fs.dataPath(info)
	if err != nil {
		return nil, nil, &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}

	file, err := os.Open(objPath)
	if err != nil {
		return nil, nil, &S3Error{Code: "NoSuchKey", Message: errKeyNotFound}
	}

	var closers []io.Closer
	closers = append(closers, file)
	var reader io.Reader = file

	var ssecParams *SSECParams
	if ctx != nil {
		if params, ok := ctx.Value(SSECContextKey).(*SSECParams); ok && params != nil {
			ssecParams = params
		}
	}

	if info.SSECustomerAlgorithm != "" && ssecParams != nil {
		iv := make([]byte, 16)
		if _, err := io.ReadFull(file, iv); err != nil {
			file.Close()
			return nil, nil, fmt.Errorf("failed to read IV: %w", err)
		}
		block, err := aes.NewCipher(ssecParams.Key)
		if err != nil {
			file.Close()
			return nil, nil, fmt.Errorf(errFailedCreateAES, err)
		}
		crs, err := newCTRReadSeeker(file, block, iv)
		if err != nil {
			file.Close()
			return nil, nil, fmt.Errorf("failed to initialize seekable decryption: %w", err)
		}
		reader = crs
	}

	if info.Compressed {
		gzipReader, err := getGzipReader(reader)
		if err != nil {
			file.Close()
			return nil, nil, fmt.Errorf("failed to initialize gzip reader: %w", err)
		}
		pReader := &pooledGzipReader{Reader: gzipReader}
		closers = append(closers, pReader)
		reader = pReader
	}

	rc := &readCloserWrapper{Reader: reader, closers: closers}
	if reader == io.Reader(file) {
		// Neither encrypted nor compressed: the bytes on disk are the object.
		return &fileObjectReader{
			metricsReadSeekCloser: metricsReadSeekCloser{
				metricsReadCloser: metricsReadCloser{ReadCloser: rc, metrics: fs.Metrics()},
				seeker:            file,
			},
			file: file,
		}, info, nil
	}
	if rs, ok := reader.(io.ReadSeeker); ok {
		return &metricsReadSeekCloser{
			metricsReadCloser: metricsReadCloser{ReadCloser: rc, metrics: fs.Metrics()},
			seeker:            rs,
		}, info, nil
	}
	return &metricsReadCloser{ReadCloser: rc, metrics: fs.Metrics()}, info, nil
}

// HeadObject retrieves object metadata without the data.
func (fs *FilesystemEngine) HeadObject(ctx context.Context, bucket, key, versionID string) (*ObjectInfo, error) {
	if err := fs.validateBucketName(bucket); err != nil {
		return nil, err
	}
	if _, err := fs.objectPathWithVersion(bucket, key, versionID); err != nil {
		return nil, &S3Error{Code: "InvalidArgument", Message: err.Error()}
	}

	info, err := fs.metadata.GetObjectMeta(bucket, key, versionID)
	if err != nil {
		return nil, &S3Error{Code: "NoSuchKey", Message: errKeyNotFound}
	}

	if info.IsDeleteMarker {
		return info, &S3Error{Code: "NoSuchKey", Message: errKeyNotFound}
	}

	ssecParams := extractSSECParams(ctx)
	if err := validateSSECParams(info, ssecParams); err != nil {
		return info, err
	}

	return info, nil
}

// DeleteObject removes an object version or creates a delete marker.
func (fs *FilesystemEngine) DeleteObject(bucket, key, versionID string) (isDeleteMarker bool, delVersionID string, err error) {
	if err := fs.requireBucket(bucket); err != nil {
		return false, "", err
	}

	// The same per-key lock PutObject and CompleteMultipartUpload hold while
	// they rename and write metadata. Without it a delete landing between a
	// PUT's rename and its metadata write removed the new file and left
	// metadata pointing at nothing.
	unlockObj := fs.lockObjectKey(bucket, key)
	defer unlockObj()

	versionStatus, err := fs.metadata.GetBucketVersioning(bucket)
	if err != nil {
		versionStatus = ""
	}

	// Throughout, metadata is removed before the data it points at: a crash
	// in between leaves an unreferenced file, never a record whose data is
	// gone.
	if versionID != "" {
		if _, err := fs.objectPathWithVersion(bucket, key, versionID); err != nil {
			return false, "", &S3Error{Code: "InvalidArgument", Message: err.Error()}
		}

		// Whether the version being removed is the current one decides what
		// the (unversioned) mirror needs, so capture it before deleting.
		latest, latestErr := fs.metadata.GetObjectMeta(bucket, key, "")
		wasLatest := latestErr == nil && latest.VersionID == versionID
		removed, _ := fs.storedRecord(bucket, key, versionID)

		err = fs.metadata.DeleteObjectMeta(bucket, key, versionID)
		if err != nil {
			return false, "", err
		}
		fs.removeData(removed)

		info := &ObjectInfo{
			Bucket:    bucket,
			Key:       key,
			VersionID: versionID,
		}
		fs.triggerWebhook("ObjectRemoved:Delete", info)
		fs.mirrorAfterVersionDelete(bucket, key, wasLatest)
		return false, "", nil
	}

	if versionStatus == "Enabled" || versionStatus == "Suspended" {
		if err := fs.promoteLegacyNullVersion(bucket, key); err != nil {
			return false, "", err
		}
		var delVersionID string
		var replaced *ObjectInfo
		switch versionStatus {
		case "Enabled":
			delVersionID = uuid.New().String()
		case "Suspended":
			// The delete marker becomes the null version, replacing it.
			delVersionID = "null"
			replaced, _ = fs.storedRecord(bucket, key, "null")
		}

		info := &ObjectInfo{
			Bucket:         bucket,
			Key:            key,
			Size:           0,
			ETag:           "",
			ContentType:    "",
			LastModified:   time.Now().UTC(),
			VersionID:      delVersionID,
			IsDeleteMarker: true,
		}

		if err := fs.metadata.PutObjectMeta(info); err != nil {
			return false, "", err
		}
		fs.removeData(replaced)

		fs.triggerWebhook("ObjectRemoved:DeleteMarkerCreated", info)
		fs.MirrorSync(bucket, key, "DELETE")
		return true, delVersionID, nil
	}

	info, err := fs.metadata.GetObjectMeta(bucket, key, "")
	if err != nil {
		// Deleting a key that isn't there is a success in S3: DELETE is
		// idempotent and returns 204 either way.
		return false, "", nil //nolint:nilerr // intentional: absent key is not an error
	}

	// Every version's data goes with the key: collect it before the
	// metadata that locates it is deleted.
	versions, err := fs.metadata.GetObjectVersions(bucket, key)
	if err != nil {
		versions = []ObjectInfo{*info}
	}

	err = fs.metadata.DeleteObjectMeta(bucket, key, "")
	if err != nil {
		return false, "", err
	}
	for i := range versions {
		fs.removeData(&versions[i])
	}

	fs.triggerWebhook("ObjectRemoved:Delete", info)
	fs.MirrorSync(bucket, key, "DELETE")
	return false, "", nil
}

// commitObject publishes a fully written temp file as the data of info, a
// new version of info.Key, and records its metadata. Callers hold the key
// lock and the shared bucket lock.
//
// The data goes to a new blob, so nothing is overwritten in place: the record
// this write replaces (see prepareKeyForWrite) has its data removed only once
// the new metadata is committed. A crash in between leaves an unreferenced
// file, never a record pointing at the wrong or missing data.
func (fs *FilesystemEngine) commitObject(tmpPath, blobID, blobPath string, info *ObjectInfo) error {
	replaced, err := fs.prepareKeyForWrite(info.Bucket, info.Key, info.VersionID)
	if err != nil {
		return err
	}
	if err := os.Rename(tmpPath, blobPath); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}
	info.BlobID = blobID
	if err := fs.metadata.PutObjectMeta(info); err != nil {
		removeObjectFile(blobPath) // never referenced by any record
		return err
	}
	fs.removeData(replaced)
	return nil
}

// prepareKeyForWrite reconciles what is already stored under key with the
// version about to be committed, and returns the record that version
// replaces, if any. Callers hold the key lock.
//
// An unversioned write replaces the current object and drops the key's
// version history, whose data is removed here before its metadata goes in
// PutObjectMeta. A versioned write first turns an object written before
// versioning was enabled into the "null" version, as S3 does; with
// versioning suspended the write then replaces that null version.
func (fs *FilesystemEngine) prepareKeyForWrite(bucket, key, versionID string) (*ObjectInfo, error) {
	if versionID == "" {
		fs.removeVersionFiles(bucket, key)
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
	if err != nil || rec.IsDeleteMarker || rec.VersionID != versionID {
		return nil, nil //nolint:nilerr // no record, or not the one asked for: nothing is replaced
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
// key-path layout is moved to the null version's path.
func (fs *FilesystemEngine) promoteLegacyNullVersion(bucket, key string) error {
	latest, err := fs.metadata.GetObjectMeta(bucket, key, "")
	if err != nil || latest.VersionID != "" || latest.IsDeleteMarker {
		return nil //nolint:nilerr // nothing stored, or already versioned: nothing to promote
	}

	if latest.BlobID == "" {
		src, err := fs.objectPathWithVersion(bucket, key, "")
		if err != nil {
			return &S3Error{Code: "InvalidArgument", Message: err.Error()}
		}
		dst, err := fs.objectPathWithVersion(bucket, key, "null")
		if err != nil {
			return &S3Error{Code: "InvalidArgument", Message: err.Error()}
		}
		if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to move pre-versioning object to its null version: %w", err)
		}
	}

	latest.VersionID = "null"
	latest.IsLatest = true
	return fs.metadata.PutObjectMetaRaw(latest)
}

// removeVersionFiles deletes the data of every stored version of key, leaving
// the unversioned object's own data alone. Callers hold the key lock.
func (fs *FilesystemEngine) removeVersionFiles(bucket, key string) {
	versions, err := fs.metadata.GetObjectVersions(bucket, key)
	if err != nil {
		return
	}
	for i := range versions {
		if versions[i].VersionID != "" {
			fs.removeData(&versions[i])
		}
	}
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

// CopyObject copies an object from one location to another.
//
// srcVersionID selects a specific source version; it is passed as a real
// argument rather than being string-split out of the key, which used to corrupt
// any key containing the literal "?versionId=".
//
// ctx carries the SSE-C parameters for both halves of the copy, so encrypted
// sources and destinations are now supported.
func (fs *FilesystemEngine) CopyObject(ctx context.Context, src CopySource, dstBucket, dstKey string) (*ObjectInfo, error) {
	if err := fs.validateBucketName(src.Bucket); err != nil {
		return nil, err
	}
	if err := fs.validateBucketName(dstBucket); err != nil {
		return nil, err
	}

	if ctx == nil {
		ctx = context.Background()
	}

	// The source and destination may use different SSE-C keys (or none), so the
	// source key is set explicitly rather than inherited from ctx. A typed nil
	// clears any destination key the caller already attached.
	var srcCtx context.Context
	if src.SSEC != nil {
		srcCtx = context.WithValue(ctx, SSECContextKey, src.SSEC)
	} else {
		srcCtx = context.WithValue(ctx, SSECContextKey, (*SSECParams)(nil))
	}

	reader, srcInfo, err := fs.GetObject(srcCtx, src.Bucket, src.Key, src.VersionID)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	return fs.PutObject(ctx, dstBucket, dstKey, reader, srcInfo.Size, srcInfo.ContentType)
}

// GetBucketVersioning gets the versioning status of a bucket.
func (fs *FilesystemEngine) GetBucketVersioning(bucket string) (string, error) {
	if err := fs.requireBucket(bucket); err != nil {
		return "", err
	}
	return fs.metadata.GetBucketVersioning(bucket)
}

// SetBucketVersioning sets the versioning status of a bucket.
func (fs *FilesystemEngine) SetBucketVersioning(bucket, status string) error {
	if err := fs.requireBucket(bucket); err != nil {
		return err
	}
	if status != "Enabled" && status != "Suspended" && status != "Disabled" {
		return &S3Error{Code: "InvalidArgument", Message: "Versioning status must be Enabled, Suspended, or Disabled"}
	}
	if status == "Disabled" {
		// As in S3, a versioned bucket can be suspended but never returned to
		// unversioned. Allowing it sent the next plain DELETE down the
		// unversioned path, which erases every version of the key.
		current, err := fs.metadata.GetBucketVersioning(bucket)
		if err != nil {
			return err
		}
		if current == "Enabled" || current == "Suspended" {
			return &S3Error{
				Code:    "IllegalVersioningConfigurationException",
				Message: "A versioned bucket cannot be returned to an unversioned state; use Suspended instead.",
			}
		}
		// Never versioned: Disabled is a no-op.
		return nil
	}
	return fs.metadata.PutBucketVersioning(bucket, status)
}

// SetBucketPublic sets the public status of a bucket.
func (fs *FilesystemEngine) SetBucketPublic(bucket string, public bool) error {
	if err := fs.requireBucket(bucket); err != nil {
		return err
	}
	return fs.metadata.SetBucketPublic(bucket, public)
}

// IsBucketPublic gets the public status of a bucket.
func (fs *FilesystemEngine) IsBucketPublic(bucket string) (bool, error) {
	if err := fs.requireBucket(bucket); err != nil {
		return false, err
	}
	return fs.metadata.IsBucketPublic(bucket)
}

// ListObjects lists objects in a bucket with prefix, delimiter, and pagination support.
// Performs cursor-based pagination and skip-scanning direct query inside bbolt transaction
// to avoid loading all keys in memory (OOM safety).
func (fs *FilesystemEngine) ListObjects(input *ListObjectsInput) (*ListObjectsOutput, error) {
	if err := fs.requireBucket(input.Bucket); err != nil {
		return nil, err
	}

	maxKeys := input.MaxKeys
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	// Determine the start position
	startAfter := input.StartAfter
	if input.ContinuationToken != "" {
		startAfter = input.ContinuationToken
	}

	var objects []ObjectInfo
	var commonPrefixes []string
	commonPrefixSet := make(map[string]bool)

	bucketPrefix := input.Bucket + "\x00"

	// Seek to whichever of the prefix and the marker sorts later.
	//
	// The marker used to win outright whenever it was set, so a marker that
	// sorted before the prefix — an ordinary start-after/marker value, since
	// callers choose it freely — put the cursor ahead of keys the prefix
	// filter then rejected. The scan breaks on the first non-matching key, so
	// the whole page came back empty even with matching keys further on.
	seekKey := bucketPrefix
	if input.Prefix != "" {
		seekKey = bucketPrefix + input.Prefix
	}
	if startAfter != "" {
		afterKey := bucketPrefix + startAfter + "\x00"
		if input.Delimiter != "" && strings.HasSuffix(startAfter, input.Delimiter) {
			afterKey = bucketPrefix + startAfter + "\xff"
		}
		seekKey = advanceToken(seekKey, afterKey)
	}

	isTruncated := false
	nextToken := ""

	db, releasedb, err := fs.metadata.acquireBucketDB(input.Bucket)
	if err != nil {
		return nil, err
	}
	defer releasedb()

	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucket)
		c := b.Cursor()

		k, v := c.Seek([]byte(seekKey))
		for k != nil {
			if !strings.HasPrefix(string(k), bucketPrefix) {
				break
			}

			objectKey := string(k[len(bucketPrefix):])

			if strings.Contains(objectKey, "\x00") {
				k, v = c.Next()
				continue
			}

			if startAfter != "" && objectKey <= startAfter {
				k, v = c.Next()
				continue
			}

			if input.Prefix != "" {
				if !strings.HasPrefix(objectKey, input.Prefix) {
					break
				}
			}

			var obj ObjectInfo
			if err := json.Unmarshal(v, &obj); err != nil {
				return err
			}
			if obj.IsDeleteMarker {
				k, v = c.Next()
				continue
			}

			if input.Delimiter != "" {
				remaining := objectKey[len(input.Prefix):]
				delimIdx := strings.Index(remaining, input.Delimiter)
				if delimIdx >= 0 {
					dirPrefix := input.Prefix + remaining[:delimIdx+len(input.Delimiter)]
					if !commonPrefixSet[dirPrefix] {
						if len(objects)+len(commonPrefixes) >= maxKeys {
							isTruncated = true
							break
						}
						commonPrefixSet[dirPrefix] = true
						commonPrefixes = append(commonPrefixes, dirPrefix)
						// The continuation token must advance past every item
						// already emitted, prefixes included. Tracking only the
						// last object made the token point backwards whenever a
						// page ended on a prefix, so the next page re-emitted
						// those prefixes and paging could never make progress.
						nextToken = advanceToken(nextToken, dirPrefix)
					}
					nextSeekKey := bucketPrefix + dirPrefix + "\xff"
					k, v = c.Seek([]byte(nextSeekKey))
					continue
				}
			}

			if len(objects)+len(commonPrefixes) >= maxKeys {
				isTruncated = true
				break
			}

			objects = append(objects, obj)
			nextToken = advanceToken(nextToken, obj.Key)

			k, v = c.Next()
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	sort.Strings(commonPrefixes)

	if !isTruncated {
		nextToken = ""
	}

	return &ListObjectsOutput{
		Objects:               objects,
		CommonPrefixes:        commonPrefixes,
		IsTruncated:           isTruncated,
		NextContinuationToken: nextToken,
		KeyCount:              len(objects) + len(commonPrefixes),
	}, nil
}

// advanceToken returns whichever of the two continuation-token candidates sorts
// later. Listing walks keys in ascending order, so the token must always be the
// greatest item emitted so far — object key or rolled-up common prefix alike.
func advanceToken(current, candidate string) string {
	if candidate > current {
		return candidate
	}
	return current
}

// S3Error represents an S3 API error with a code and message.
type S3Error struct {
	Code    string
	Message string
}

func (e *S3Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

type readCloserWrapper struct {
	io.Reader
	closers []io.Closer
}

func (w *readCloserWrapper) Close() error {
	var firstErr error
	for i := len(w.closers) - 1; i >= 0; i-- {
		if err := w.closers[i].Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// maxCompressibleSize is the largest object Stiva will gzip on write.
//
// A compressed object cannot be served with a seekable reader, so every Range
// request against one has to decompress from byte zero to reach the requested
// offset — O(n) per request, on exactly the large text and log files where
// ranged reads are most useful. Below this threshold the whole object is cheap
// to decompress anyway; above it, keeping the object seekable is worth more
// than the disk saved.
//
// This only affects newly written objects. Existing ones carry Compressed in
// their metadata and continue to be read back correctly either way.
const maxCompressibleSize = 8 << 20 // 8MiB

// shouldCompress reports whether an object of this type and size is worth
// gzipping. A size of -1 means the length is not known up front (a streamed or
// chunked upload), which is characteristic of large uploads, so those are left
// uncompressed and seekable.
func shouldCompress(contentType string, size int64) bool {
	if !isCompressibleContentType(contentType) {
		return false
	}
	return size >= 0 && size <= maxCompressibleSize
}

func isCompressibleContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if idx := strings.Index(contentType, ";"); idx != -1 {
		contentType = contentType[:idx]
		contentType = strings.TrimSpace(contentType)
	}
	if strings.HasPrefix(contentType, "text/") {
		return true
	}
	switch contentType {
	case "application/json", "application/xml", "application/javascript", "application/x-javascript", "image/svg+xml":
		return true
	}
	return false
}

type metricsReadCloser struct {
	io.ReadCloser
	metrics *MetricsTracker
}

func (m *metricsReadCloser) Read(p []byte) (int, error) {
	n, err := m.ReadCloser.Read(p)
	m.metrics.AddDownloaded(int64(n))
	return n, err
}

type metricsReadSeekCloser struct {
	metricsReadCloser
	seeker io.ReadSeeker
}

func (m *metricsReadSeekCloser) Seek(offset int64, whence int) (int64, error) {
	return m.seeker.Seek(offset, whence)
}

// FileBacked is implemented by object readers whose content is exactly the
// bytes of a file on disk — neither encrypted nor compressed. An HTTP layer
// can then hand the file itself to net/http, which copies it with sendfile
// instead of through userspace. It must count what it sends with
// CountDownloaded, since reads of the file bypass the reader's own metrics.
type FileBacked interface {
	BackingFile() *os.File
	CountDownloaded(n int64)
}

// fileObjectReader is the reader GetObject returns for a plain object.
type fileObjectReader struct {
	metricsReadSeekCloser
	file *os.File
}

func (f *fileObjectReader) BackingFile() *os.File { return f.file }

func (f *fileObjectReader) CountDownloaded(n int64) { f.metrics.AddDownloaded(n) }

// Metadata returns the underlying MetadataStore.
func (fs *FilesystemEngine) Metadata() *MetadataStore {
	return fs.metadata
}

// DataDir returns the root data directory path.
func (fs *FilesystemEngine) DataDir() string {
	return fs.dataDir
}

func extractSSECParams(ctx context.Context) *SSECParams {
	if ctx == nil {
		return nil
	}
	if params, ok := ctx.Value(SSECContextKey).(*SSECParams); ok {
		return params
	}
	return nil
}

func validateSSECParams(info *ObjectInfo, params *SSECParams) error {
	if info.SSECustomerAlgorithm != "" {
		if params == nil {
			return &S3Error{
				Code:    "InvalidArgument",
				Message: "The object was stored using a Server-side Encryption with Customer-provided Keys (SSE-C) and cannot be retrieved without it",
			}
		}
		if subtle.ConstantTimeCompare([]byte(params.KeyMD5), []byte(info.SSECustomerKeyMD5)) != 1 {
			return &S3Error{
				Code:    "InvalidDigest",
				Message: "The customer-provided encryption key MD5 does not match",
			}
		}
	} else if params != nil {
		return &S3Error{
			Code:    "InvalidArgument",
			Message: "The object was not stored using a Server-side Encryption with Customer-provided Keys (SSE-C) and cannot be retrieved with it",
		}
	}
	return nil
}

// seedActiveMultipartGauge starts the in-progress multipart gauge from the
// uploads already recorded. It used to start at zero on every boot, so
// completing or aborting uploads begun before a restart drove it negative.
func seedActiveMultipartGauge(meta *MetadataStore, metrics *MetricsTracker) {
	buckets, err := meta.ListBuckets()
	if err != nil {
		slog.Warn("[Storage] Could not count in-progress multipart uploads at startup", "error", err)
		return
	}
	var total int64
	for _, b := range buckets {
		err := meta.IterateMultipartMetas(b.Name, func(*MultipartMeta) error {
			total++
			return nil
		})
		if err != nil {
			slog.Warn("[Storage] Could not count in-progress multipart uploads", "bucket", b.Name, "error", err)
		}
	}
	metrics.SetActiveMultiparts(total)
}

func cleanupOrphanedTempFiles(dataDir string) {
	walkDirs := []string{
		filepath.Join(dataDir, "buckets"),
		filepath.Join(dataDir, "blobs"),
		filepath.Join(dataDir, "multipart"),
		filepath.Join(dataDir, "tmp"),
	}
	for _, dir := range walkDirs {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				// Skip anything unreadable rather than abandoning the sweep;
				// this is best-effort startup cleanup.
				return nil //nolint:nilerr // intentional: keep walking past unreadable entries
			}
			// Only regular files are removed: skipping symlinks avoids the
			// TOCTOU traversal a symlinked temp name could otherwise cause.
			if d.Type().IsRegular() {
				name := d.Name()
				if strings.HasPrefix(name, ".stiva-tmp-") || strings.HasPrefix(name, ".part-tmp-") || strings.HasPrefix(name, ".stiva-multipart-") || strings.HasPrefix(name, "stiva-body-") || strings.HasPrefix(name, "stiva-chunked-") {
					_ = os.Remove(path) //nolint:gosec // startup sweep over our own data dir; symlinks excluded above
				}
			}
			return nil
		})
	}
}

type ctrReadSeeker struct {
	file          *os.File
	block         cipher.Block
	iv            []byte
	currentOffset int64
	stream        cipher.Stream
}

func newCTRReadSeeker(file *os.File, block cipher.Block, iv []byte) (*ctrReadSeeker, error) {
	crs := &ctrReadSeeker{
		file:  file,
		block: block,
		iv:    iv,
	}
	_, err := crs.Seek(0, io.SeekStart)
	if err != nil {
		return nil, err
	}
	return crs, nil
}

func (crs *ctrReadSeeker) Read(p []byte) (int, error) {
	if crs.stream == nil {
		return 0, io.EOF
	}
	n, err := crs.file.Read(p)
	if n > 0 {
		crs.stream.XORKeyStream(p[:n], p[:n])
		crs.currentOffset += int64(n)
	}
	return n, err
}

func (crs *ctrReadSeeker) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = crs.currentOffset + offset
	case io.SeekEnd:
		stat, err := crs.file.Stat()
		if err != nil {
			return 0, err
		}
		contentSize := stat.Size() - 16
		if contentSize < 0 {
			contentSize = 0
		}
		target = contentSize + offset
	default:
		return 0, fmt.Errorf("invalid whence: %d", whence)
	}

	if target < 0 {
		return 0, fmt.Errorf("negative seek offset: %d", target)
	}

	blockIndex := target / 16
	remainder := target % 16

	ctr := make([]byte, len(crs.iv))
	copy(ctr, crs.iv)
	addCtr(ctr, uint64(blockIndex))

	_, err := crs.file.Seek(16+blockIndex*16, io.SeekStart)
	if err != nil {
		return 0, err
	}

	crs.stream = cipher.NewCTR(crs.block, ctr)
	// The file now sits at the start of target's block. The discard read
	// below advances currentOffset by remainder itself, so starting from
	// target here (as this used to) over-counted by remainder.
	crs.currentOffset = blockIndex * 16

	if remainder > 0 {
		discardBuf := make([]byte, remainder)
		_, err := io.ReadFull(crs, discardBuf)
		if err != nil {
			return 0, err
		}
	}

	return target, nil
}

func addCtr(ctr []byte, val uint64) {
	for i := len(ctr) - 1; i >= 0; i-- {
		val += uint64(ctr[i])
		ctr[i] = byte(val & 0xff) // deliberate truncation: big-endian counter arithmetic
		val >>= 8
	}
}
