package s3api

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/httpx"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

type logTask struct {
	targetBucket string
	targetPrefix string
	logLine      string
}

// RouterOptions configures an S3 API Router.
type RouterOptions struct {
	Engine           storage.Engine
	Creds            *auth.Credentials
	Region           string
	Domain           string
	TrustProxy       bool
	TrustedProxyHops int
	// LogFlushInterval controls how often buffered access logs are written.
	// Zero selects defaultLogFlushInterval.
	LogFlushInterval time.Duration
}

// Router handles S3 API request routing and dispatching.
type Router struct {
	engine           storage.Engine
	verifier         *auth.SigV4Verifier
	region           string
	domain           string
	trustProxy       bool
	trustedProxyHops int
	logFlushInterval time.Duration
	logChan          chan logTask
	logWG            sync.WaitGroup
	activeReqs       sync.WaitGroup
	closeOnce        sync.Once
}

// defaultLogFlushInterval is how often buffered access log lines are flushed.
const defaultLogFlushInterval = 5 * time.Second

// NewRouter creates a new S3 API router.
func NewRouter(opts RouterOptions) *Router {
	hops := opts.TrustedProxyHops
	if hops < 1 {
		hops = 1
	}
	flush := opts.LogFlushInterval
	if flush <= 0 {
		flush = defaultLogFlushInterval
	}

	rt := &Router{
		engine:           opts.Engine,
		verifier:         auth.NewSigV4Verifier(opts.Creds),
		region:           opts.Region,
		domain:           opts.Domain,
		trustProxy:       opts.TrustProxy,
		trustedProxyHops: hops,
		logFlushInterval: flush,
		logChan:          make(chan logTask, 1000),
	}
	rt.startLogWorkers()
	return rt
}

// metricsResponseWriter records the status code and response size.
//
// It must forward the optional interfaces net/http's ResponseWriter may also
// implement. Without ReadFrom, io.Copy on an *os.File loses the sendfile fast
// path — every object download was being copied through userspace. Without
// Flush, streamed responses buffer instead of reaching the client.
type metricsResponseWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int64
}

func (w *metricsResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *metricsResponseWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytesWritten += int64(n)
	return n, err
}

// ReadFrom preserves the sendfile path when the underlying writer supports it.
func (w *metricsResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		w.bytesWritten += n
		return n, err
	}
	n, err := io.Copy(w.ResponseWriter, src)
	w.bytesWritten += n
	return n, err
}

func (w *metricsResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *metricsResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	return hj.Hijack()
}

// Unwrap lets net/http reach the original writer (used by ResponseController).
func (w *metricsResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// ServeHTTP implements the http.Handler interface for the S3 API.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt.activeReqs.Add(1)
	defer rt.activeReqs.Done()

	// The SigV4 layer may replace r.Body with a reader backed by a temp file
	// (auth.HashPayload spools bodies over 2MiB, and decodeStreamingPayload
	// does the same for aws-chunked uploads). That reader deletes its file on
	// Close, but net/http only closes the *original* body it captured when it
	// read the request — never the replacement — so without this every signed
	// request over 2MiB left a file behind for good. Closing here covers the
	// rejected-signature path too, where the body is spooled before the
	// signature is compared. Close is idempotent and safe on the unreplaced
	// body.
	defer func() {
		if r.Body != nil {
			_ = r.Body.Close()
		}
	}()

	start := time.Now()
	mrw := &metricsResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
	storage.GlobalMetrics.IncRequests()

	// Resolved once here and threaded through: this used to be computed twice
	// per request, once for routing and again for logging.
	bucket, key := rt.resolveBucketAndKey(r)

	rt.serveHTTPInternal(mrw, r, bucket, key)

	if mrw.statusCode >= 400 {
		storage.GlobalMetrics.IncErrors()
	}

	rt.logAccess(r, bucket, key, mrw.statusCode, mrw.bytesWritten, time.Since(start))
}

func (rt *Router) logAccess(r *http.Request, bucket, key string, statusCode int, bytesSent int64, duration time.Duration) {
	if bucket == "" {
		return
	}

	logging, err := rt.engine.GetBucketLogging(bucket)
	if err != nil || logging == nil || logging.LoggingEnabled == nil {
		return
	}

	targetBucket := logging.LoggingEnabled.TargetBucket
	targetPrefix := logging.LoggingEnabled.TargetPrefix

	if bucket == targetBucket && strings.HasPrefix(key, targetPrefix) {
		return // Skip to avoid infinite loop
	}

	owner := "stiva"
	timeStr := time.Now().Format("02/Jan/2006:15:04:05 -0700")
	// Reading the leftmost X-Forwarded-For entry let any caller forge the
	// source address recorded here; httpx.ClientIP reads from the trusted end.
	remoteIP := httpx.ClientIP(r, rt.trustProxy, rt.trustedProxyHops)

	requester := "-"
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "AWS4-HMAC-SHA256") {
		parts := strings.Split(authHeader, ",")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "Credential=") {
				cred := strings.TrimPrefix(part, "Credential=")
				credParts := strings.Split(cred, "/")
				if len(credParts) > 0 {
					requester = credParts[0]
				}
			}
		}
	}

	requestID := "stiva"
	var operation string
	if key != "" {
		operation = "REST." + r.Method + ".OBJECT"
	} else {
		operation = "REST." + r.Method + ".BUCKET"
	}

	reqURI := fmt.Sprintf("%s %s %s", r.Method, r.URL.RequestURI(), r.Proto)

	errCode := "-"
	if statusCode >= 400 {
		errCode = http.StatusText(statusCode)
	}

	referrer := r.Referer()
	if referrer == "" {
		referrer = "-"
	}

	userAgent := r.UserAgent()
	if userAgent == "" {
		userAgent = "-"
	}

	logLine := fmt.Sprintf("%s %s [%s] %s %s %s %s %s \"%s\" %d %s %d - %d %d \"%s\" \"%s\" -\n",
		owner, bucket, timeStr, remoteIP, requester, requestID, operation, key, reqURI, statusCode, errCode, bytesSent, duration.Milliseconds(), duration.Milliseconds(), referrer, userAgent,
	)

	task := logTask{
		targetBucket: targetBucket,
		targetPrefix: targetPrefix,
		logLine:      logLine,
	}
	select {
	case rt.logChan <- task:
	default:
		slog.Warn("[S3 API] Access log queue full, dropping log", "bucket", targetBucket)
	}
}

func (rt *Router) serveHTTPInternal(w http.ResponseWriter, r *http.Request, bucket, key string) {
	// Set common S3 response headers
	w.Header().Set("Server", "Stiva")
	w.Header().Set("x-amz-request-id", "stiva")

	// Apply CORS headers on all matching requests (preflight or normal)
	if bucket != "" {
		hasCORS := rt.handleCORS(w, r, bucket)
		if r.Method == http.MethodOptions {
			if hasCORS {
				return // Preflight completed successfully
			}
			w.WriteHeader(http.StatusForbidden)
			return
		}
	} else if r.Method == http.MethodOptions {
		// OPTIONS request at service level is not supported
		writeS3Error(w, "MethodNotAllowed", "Method not allowed", r.URL.Path)
		return
	}

	// Authenticate the request (bypass for OPTIONS or public bucket object reads)
	bypassAuth := false
	if bucket != "" && key != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		if public, err := rt.engine.IsBucketPublic(bucket); err == nil && public {
			bypassAuth = true
		}
	}

	if !bypassAuth {
		if err := rt.verifier.Verify(r); err != nil {
			slog.Warn("[S3] Auth failed", "method", r.Method, "path", r.URL.Path, "error", err)
			var authErr *auth.AuthError
			if errors.As(err, &authErr) {
				writeS3Error(w, authErr.Code, authErr.Message, r.URL.Path)
			} else {
				writeS3Error(w, "AccessDenied", "Access Denied", r.URL.Path)
			}
			return
		}
	}

	if bucket != "" && !storage.IsValidBucketName(bucket) {
		writeS3Error(w, "InvalidBucketName", "The specified bucket is not valid.", "/"+bucket)
		return
	}

	slog.Info("[S3] Request received", "method", r.Method, "path", r.URL.Path, "bucket", bucket, "key", key)

	// Route based on path structure and method
	if bucket == "" {
		// Service-level operations
		rt.handleServiceOps(w, r)
		return
	}

	if key == "" {
		// Bucket-level operations
		rt.handleBucketOps(w, r, bucket)
		return
	}

	// Object-level operations
	rt.handleObjectOps(w, r, bucket, key)
}

// ResolveBucketAndKey resolves the bucket and key from an HTTP request.
func (rt *Router) ResolveBucketAndKey(r *http.Request) (string, string) {
	return rt.resolveBucketAndKey(r)
}

func (rt *Router) resolveBucketAndKey(r *http.Request) (string, string) {
	host := r.Host
	if strings.Contains(host, ":") {
		h, _, err := net.SplitHostPort(host)
		if err == nil {
			host = h
		}
	}

	path := strings.TrimPrefix(r.URL.Path, "/")

	if rt.domain != "" && strings.HasSuffix(host, "."+rt.domain) {
		bucket := strings.TrimSuffix(host, "."+rt.domain)
		if bucket != "" && !strings.Contains(bucket, ".") {
			return bucket, path
		}
	}

	// Fallback to path style routing
	parts := strings.SplitN(path, "/", 2)
	bucket := ""
	key := ""
	if len(parts) > 0 {
		bucket = parts[0]
	}
	if len(parts) > 1 {
		key = parts[1]
	}
	return bucket, key
}

func (rt *Router) handleCORS(w http.ResponseWriter, r *http.Request, bucket string) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}

	// The response varies by Origin whether or not a rule matches. Without this
	// a shared cache or CDN can hand one origin's Access-Control-Allow-Origin
	// to a different origin.
	w.Header().Add("Vary", "Origin")

	cors, err := rt.engine.GetBucketCORS(bucket)
	if err != nil || cors == nil {
		return false
	}

	headers, matched := EvaluateCORS(r, cors)
	if matched {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
		}
		return true
	}
	return false
}

// handleServiceOps handles service-level operations (e.g., ListBuckets).
func (rt *Router) handleServiceOps(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rt.handleListBuckets(w, r)
	default:
		writeS3Error(w, "MethodNotAllowed", "Method not allowed", "/")
	}
}

// HandleBucketOps handles bucket-level operations.
func (rt *Router) HandleBucketOps(w http.ResponseWriter, r *http.Request, bucket string) {
	rt.handleBucketOps(w, r, bucket)
}

// handleBucketOps handles bucket-level operations.
//
// Each branch declares the subresources it implements; anything else known to
// S3 is rejected with NotImplemented rather than falling through to the
// method's default (and, for DELETE, destructive) operation.
func (rt *Router) handleBucketOps(w http.ResponseWriter, r *http.Request, bucket string) {
	query := r.URL.Query()
	resource := "/" + bucket

	switch r.Method {
	case http.MethodGet:
		switch {
		case query.Has("cors"):
			rt.handleGetBucketCORS(w, r, bucket)
		case query.Has("versioning"):
			rt.handleGetBucketVersioning(w, r, bucket)
		case query.Has("lifecycle"):
			rt.handleGetBucketLifecycle(w, r, bucket)
		case query.Has("logging"):
			rt.handleGetBucketLogging(w, r, bucket)
		case query.Has("location"):
			rt.handleGetBucketLocation(w, r, bucket)
		case query.Has("versions"):
			rt.handleListObjectVersions(w, r, bucket)
		case query.Has("uploads"):
			rt.handleListMultipartUploads(w, r, bucket)
		case rejectUnhandledSubresource(w, query, resource):
			return
		default:
			rt.handleListObjects(w, r, bucket)
		}
	case http.MethodPut:
		switch {
		case query.Has("cors"):
			rt.handlePutBucketCORS(w, r, bucket)
		case query.Has("versioning"):
			rt.handlePutBucketVersioning(w, r, bucket)
		case query.Has("lifecycle"):
			rt.handlePutBucketLifecycle(w, r, bucket)
		case query.Has("logging"):
			rt.handlePutBucketLogging(w, r, bucket)
		case rejectUnhandledSubresource(w, query, resource):
			return
		default:
			rt.handleCreateBucket(w, r, bucket)
		}
	case http.MethodPost:
		switch {
		case query.Has("delete"):
			rt.handleDeleteObjects(w, r, bucket)
		case rejectUnhandledSubresource(w, query, resource):
			return
		default:
			writeS3Error(w, "MethodNotAllowed", "Method not allowed", resource)
		}
	case http.MethodDelete:
		switch {
		case query.Has("cors"):
			rt.handleDeleteBucketCORS(w, r, bucket)
		case query.Has("lifecycle"):
			rt.handleDeleteBucketLifecycle(w, r, bucket)
		case rejectUnhandledSubresource(w, query, resource):
			return
		default:
			rt.handleDeleteBucket(w, r, bucket)
		}
	case http.MethodHead:
		if rejectUnhandledSubresource(w, query, resource) {
			return
		}
		rt.handleHeadBucket(w, r, bucket)
	default:
		writeS3Error(w, "MethodNotAllowed", "Method not allowed", resource)
	}
}

// handleObjectOps handles object-level operations. See handleBucketOps for why
// unhandled subresources must be rejected rather than falling through.
// HandleObjectOps handles object-level operations.
func (rt *Router) HandleObjectOps(w http.ResponseWriter, r *http.Request, bucket, key string) {
	rt.handleObjectOps(w, r, bucket, key)
}

func (rt *Router) handleObjectOps(w http.ResponseWriter, r *http.Request, bucket, key string) {
	query := r.URL.Query()
	resource := "/" + bucket + "/" + key

	switch r.Method {
	case http.MethodGet:
		switch {
		case query.Has("uploadId"):
			rt.handleListParts(w, r, bucket, key)
		case rejectUnhandledSubresource(w, query, resource):
			return
		default:
			rt.handleGetObject(w, r, bucket, key)
		}
	case http.MethodPut:
		switch {
		case r.Header.Get("x-amz-copy-source") != "" && query.Get("partNumber") != "" && query.Get("uploadId") != "":
			rt.handleUploadPartCopy(w, r, bucket, key)
		case r.Header.Get("x-amz-copy-source") != "":
			if rejectUnhandledSubresource(w, query, resource) {
				return
			}
			rt.handleCopyObject(w, r, bucket, key)
		case query.Get("partNumber") != "" && query.Get("uploadId") != "":
			rt.handleUploadPart(w, r, bucket, key)
		case rejectUnhandledSubresource(w, query, resource):
			return
		default:
			rt.handlePutObject(w, r, bucket, key)
		}
	case http.MethodDelete:
		switch {
		case query.Get("uploadId") != "":
			rt.handleAbortMultipartUpload(w, r, bucket, key)
		case rejectUnhandledSubresource(w, query, resource):
			return
		default:
			rt.handleDeleteObject(w, r, bucket, key)
		}
	case http.MethodHead:
		if rejectUnhandledSubresource(w, query, resource) {
			return
		}
		rt.handleHeadObject(w, r, bucket, key)
	case http.MethodPost:
		switch {
		case query.Has("uploads"):
			rt.handleCreateMultipartUpload(w, r, bucket, key)
		case query.Get("uploadId") != "":
			rt.handleCompleteMultipartUpload(w, r, bucket, key)
		case rejectUnhandledSubresource(w, query, resource):
			return
		default:
			writeS3Error(w, "MethodNotAllowed", "Method not allowed", resource)
		}
	default:
		writeS3Error(w, "MethodNotAllowed", "Method not allowed", resource)
	}
}

func (rt *Router) handleGetBucketCORS(w http.ResponseWriter, _ *http.Request, bucket string) {
	cors, err := rt.engine.GetBucketCORS(bucket)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}

	if cors == nil {
		writeS3Error(w, "NoSuchCORSConfiguration", "The CORS configuration does not exist", "/"+bucket)
		return
	}

	// Map storage CORS to XML CORS
	var rulesXML []CORSRuleXML
	for _, rule := range cors.CORSRules {
		rulesXML = append(rulesXML, CORSRuleXML{
			AllowedHeader: rule.AllowedHeaders,
			AllowedMethod: rule.AllowedMethods,
			AllowedOrigin: rule.AllowedOrigins,
			ExposeHeader:  rule.ExposeHeaders,
			MaxAgeSeconds: rule.MaxAgeSeconds,
		})
	}

	result := CORSConfigurationXML{
		Xmlns:     s3XmlNamespace,
		CORSRules: rulesXML,
	}

	writeXML(w, http.StatusOK, result)
}

func (rt *Router) handlePutBucketCORS(w http.ResponseWriter, r *http.Request, bucket string) {
	var reqBody CORSConfigurationXML
	if err := xml.NewDecoder(io.LimitReader(r.Body, maxXMLRequestBody)).Decode(&reqBody); err != nil {
		writeS3Error(w, "MalformedXML", "The XML you provided was not well-formed", "/"+bucket)
		return
	}

	// Map XML CORS to storage CORS
	var rules []storage.CORSRule
	for _, rule := range reqBody.CORSRules {
		rules = append(rules, storage.CORSRule{
			AllowedHeaders: rule.AllowedHeader,
			AllowedMethods: rule.AllowedMethod,
			AllowedOrigins: rule.AllowedOrigin,
			ExposeHeaders:  rule.ExposeHeader,
			MaxAgeSeconds:  rule.MaxAgeSeconds,
		})
	}

	cors := &storage.CORSConfiguration{
		CORSRules: rules,
	}

	err := rt.engine.PutBucketCORS(bucket, cors)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (rt *Router) handleDeleteBucketCORS(w http.ResponseWriter, _ *http.Request, bucket string) {
	err := rt.engine.DeleteBucketCORS(bucket)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type bufferedLog struct {
	lines       []string
	lastCreated time.Time
}

func (rt *Router) startLogWorkers() {
	rt.logWG.Add(1)
	go func() {
		defer rt.logWG.Done()

		buffers := make(map[string]*bufferedLog) // key: targetBucket + "\x00" + targetPrefix
		ticker := time.NewTicker(rt.logFlushInterval)
		defer ticker.Stop()

		flushBucketLogs := func(key string, blog *bufferedLog) {
			if len(blog.lines) == 0 {
				return
			}
			parts := strings.SplitN(key, "\x00", 2)
			targetBucket := parts[0]
			targetPrefix := ""
			if len(parts) > 1 {
				targetPrefix = parts[1]
			}

			logContent := strings.Join(blog.lines, "")
			logKey := fmt.Sprintf("%s%s_%s.log", targetPrefix, blog.lastCreated.Format("2006-01-02-15-04-05"), uuid.New().String()[:8])

			ctx := context.Background()
			reader := strings.NewReader(logContent)
			_, err := rt.engine.PutObject(ctx, targetBucket, logKey, reader, int64(len(logContent)), "text/plain")
			if err != nil {
				slog.Error("[S3 API] Failed to deliver aggregated access log", "bucket", targetBucket, "key", logKey, "error", err)
			}
			blog.lines = nil
		}

		flushAll := func() {
			for k, blog := range buffers {
				flushBucketLogs(k, blog)
			}
		}

		for {
			select {
			case task, ok := <-rt.logChan:
				if !ok {
					flushAll()
					return
				}

				key := task.targetBucket + "\x00" + task.targetPrefix
				blog, exists := buffers[key]
				if !exists {
					blog = &bufferedLog{
						lastCreated: time.Now().UTC(),
					}
					buffers[key] = blog
				}

				if len(blog.lines) == 0 {
					blog.lastCreated = time.Now().UTC()
				}
				blog.lines = append(blog.lines, task.logLine)

				if len(blog.lines) >= 100 {
					flushBucketLogs(key, blog)
				}

			case <-ticker.C:
				flushAll()
			}
		}
	}()
}

// Close drains in-flight requests and flushes buffered access logs. It is safe
// to call more than once; a second call previously panicked on a closed channel.
func (rt *Router) Close() {
	rt.closeOnce.Do(func() {
		rt.activeReqs.Wait()
		close(rt.logChan)
		rt.logWG.Wait()
	})
}
