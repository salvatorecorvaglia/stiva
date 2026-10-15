package s3api

import (
	"bufio"
	"bytes"
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

// publicReadKey marks a request served from a public bucket without any
// signature, so handlers can withhold the parts of the API that should only
// answer to an authenticated caller.
type publicReadKeyType struct{}

var publicReadKey publicReadKeyType

// isPublicRead reports whether this request skipped signature verification
// because the bucket is public. A request that never went through the router
// (an internal or test call) is treated as signed.
func isPublicRead(r *http.Request) bool {
	v, _ := r.Context().Value(publicReadKey).(bool)
	return v
}

type logTask struct {
	targetBucket string
	targetPrefix string
	logLine      string
}

// RouterOptions configures an S3 API Router.
type RouterOptions struct {
	Engine           storage.DataStore
	Creds            *auth.Credentials
	Region           string
	Domain           string
	TrustProxy       bool
	TrustedProxyHops int
	// Metrics receives request counters. Nil selects storage.GlobalMetrics.
	Metrics *storage.MetricsTracker
	// LogFlushInterval controls how often buffered access logs are written.
	// Zero selects defaultLogFlushInterval.
	LogFlushInterval time.Duration
}

// Router handles S3 API request routing and dispatching.
type Router struct {
	engine           storage.DataStore
	verifier         *auth.SigV4Verifier
	region           string
	domain           string
	trustProxy       bool
	trustedProxyHops int
	logFlushInterval time.Duration
	metrics          *storage.MetricsTracker
	logChan          chan logTask
	// stopLogs tells the log worker to flush and exit. logChan itself is
	// never closed: a request still running after Shutdown's deadline may
	// yet log, and a send on a closed channel panics, whereas a send to a
	// full channel is simply dropped (see logAccess).
	stopLogs   chan struct{}
	logWG      sync.WaitGroup
	activeReqs inflightCounter
	closeOnce  sync.Once
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

	metrics := opts.Metrics
	if metrics == nil {
		metrics = storage.GlobalMetrics
	}

	rt := &Router{
		metrics:          metrics,
		engine:           opts.Engine,
		verifier:         auth.NewSigV4Verifier(opts.Creds),
		region:           opts.Region,
		domain:           opts.Domain,
		trustProxy:       opts.TrustProxy,
		trustedProxyHops: hops,
		logFlushInterval: flush,
		logChan:          make(chan logTask, 1000),
		stopLogs:         make(chan struct{}),
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
	src, sent := unwrapFileBacked(src)
	var n int64
	var err error
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err = rf.ReadFrom(src)
	} else {
		n, err = io.Copy(w.ResponseWriter, src)
	}
	w.bytesWritten += n
	if sent != nil {
		sent(n)
	}
	return n, err
}

// unwrapFileBacked replaces a plain object's reader — bare, or inside the
// io.LimitedReader that http.ServeContent wraps it in — with its underlying
// *os.File, the only source net/http will send with sendfile. The returned
// function must be told how many bytes were sent, to keep the outer limit and
// the download metrics right; it is nil when src was left as it was.
func unwrapFileBacked(src io.Reader) (io.Reader, func(int64)) {
	switch s := src.(type) {
	case *io.LimitedReader:
		if fb, ok := s.R.(storage.FileBacked); ok {
			return &io.LimitedReader{R: fb.BackingFile(), N: s.N}, func(n int64) {
				s.N -= n
				fb.CountDownloaded(n)
			}
		}
	case storage.FileBacked:
		return s.BackingFile(), s.CountDownloaded
	}
	return src, nil
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
	rt.activeReqs.add()
	defer rt.activeReqs.done()

	// The SigV4 layer may replace r.Body (with a buffered copy, or a reader
	// that verifies the payload as it streams). net/http only closes the
	// *original* body it captured, never a replacement, so close whatever
	// r.Body is now. Close is idempotent and safe on the unreplaced body.
	defer func() {
		if r.Body != nil {
			_ = r.Body.Close()
		}
	}()

	// One ID per request, shared by the x-amz-request-id header, any error
	// body (writeS3Error reads it back from the header) and the access log.
	w.Header().Set(requestIDHeader, newRequestID())

	// Resolved once here and threaded through: this used to be computed twice
	// per request, once for routing and again for logging.
	bucket, key := rt.resolveBucketAndKey(r)

	if isHealthCheck(r, bucket, key) {
		w.WriteHeader(http.StatusOK)
		return
	}

	start := time.Now()
	mrw := &metricsResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
	rt.metrics.IncRequests()

	rt.serveHTTPInternal(mrw, r, bucket, key)

	if mrw.statusCode >= 400 {
		rt.metrics.IncErrors()
	}

	rt.logAccess(r, bucket, key, w.Header().Get(requestIDHeader), mrw.statusCode, mrw.bytesWritten, time.Since(start))
}

// healthBucket is the path of the liveness probe. It is not a valid bucket
// name (S3 bucket names allow no underscore), so it can never shadow one.
const healthBucket = "_health"

// isHealthCheck reports whether this is the unauthenticated liveness probe:
// GET or HEAD of exactly /_health, path-style. With virtual-host routing the
// same path is an ordinary object key and resolves with a non-empty key.
func isHealthCheck(r *http.Request, bucket, key string) bool {
	return bucket == healthBucket && key == "" && !strings.HasSuffix(r.URL.Path, "/") &&
		(r.Method == http.MethodGet || r.Method == http.MethodHead)
}

func (rt *Router) logAccess(r *http.Request, bucket, key, requestID string, statusCode int, bytesSent int64, duration time.Duration) {
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

	// Reading the leftmost X-Forwarded-For entry let any caller forge the
	// source address recorded here; httpx.ClientIP reads from the trusted end.
	remoteIP := httpx.ClientIP(r, rt.trustProxy, rt.trustedProxyHops)
	requester := auth.AccessKeyFromAuthHeader(r.Header.Get("Authorization"))

	logLine := formatAccessLogLine(r, bucket, key, remoteIP, requester, requestID, statusCode, bytesSent, duration, time.Now())

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

// formatAccessLogLine renders one entry in the S3 server access log format.
//
// Almost every field is caller-controlled — the key, the access key named in
// the Authorization header (present even when authentication failed), a
// forwarded client address, the request URI, Referer and User-Agent — and
// they used to be written verbatim. A newline in any of them forged extra
// log entries, and a quote or space shifted every following field. Bare
// fields are percent-encoded (as S3 does for keys) and quoted fields escape
// quotes, backslashes and control characters, so each entry stays one line
// with a fixed set of fields.
func formatAccessLogLine(r *http.Request, bucket, key, remoteIP, requester, requestID string, statusCode int, bytesSent int64, duration time.Duration, now time.Time) string {
	operation := "REST." + r.Method + ".BUCKET"
	if key != "" {
		operation = "REST." + r.Method + ".OBJECT"
	}

	errCode := ""
	if statusCode >= 400 {
		errCode = http.StatusText(statusCode)
	}

	const owner = "stiva"
	return fmt.Sprintf("%s %s [%s] %s %s %s %s %s \"%s\" %d %s %d - %d %d \"%s\" \"%s\" -\n",
		owner,
		escapeLogToken(bucket),
		now.Format("02/Jan/2006:15:04:05 -0700"),
		escapeLogToken(remoteIP),
		escapeLogToken(requester),
		escapeLogToken(requestID),
		escapeLogToken(operation),
		escapeLogToken(key),
		escapeLogQuoted(r.Method+" "+r.URL.RequestURI()+" "+r.Proto),
		statusCode,
		escapeLogToken(errCode),
		bytesSent,
		duration.Milliseconds(),
		duration.Milliseconds(),
		escapeLogQuoted(orDash(r.Referer())),
		escapeLogQuoted(orDash(r.UserAgent())),
	)
}

// escapeLogToken renders a bare (unquoted) access log field: "-" when empty,
// otherwise percent-encoding every byte that could end or split the field.
func escapeLogToken(s string) string {
	if s == "" {
		return "-"
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f || c == '"' || c == '\\' || c == '%' {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// escapeLogQuoted renders the inside of a double-quoted access log field,
// escaping the quote, the backslash and control characters as \xHH.
func escapeLogQuoted(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < ' ' || c == 0x7f || c == '"' || c == '\\' {
			fmt.Fprintf(&b, "\\x%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (rt *Router) serveHTTPInternal(w http.ResponseWriter, r *http.Request, bucket, key string) {
	// Set common S3 response headers
	w.Header().Set("Server", "Stiva")

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
	if isAnonymousReadCandidate(r, bucket, key) {
		if public, err := rt.engine.IsBucketPublic(bucket); err == nil && public {
			bypassAuth = true
		}
	}

	if bypassAuth {
		r = r.WithContext(context.WithValue(r.Context(), publicReadKey, true))
	} else {
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

		// A large signed or aws-chunked body is verified as it is read, with
		// any failure arriving in place of io.EOF. Object and part uploads
		// read to the end before committing, so they take it as a stream.
		// Anything else may stop early (an XML decoder can) and so must see
		// the whole body verified first; none of it accepts more than
		// maxXMLRequestBody anyway.
		if auth.IsDeferred(r.Body) && !isObjectUpload(r, key) {
			if err := readVerifiedBody(r); err != nil {
				handleStorageError(w, err, r.URL.Path)
				return
			}
		}
	}

	if bucket != "" && !storage.IsValidBucketName(bucket) {
		writeS3Error(w, "InvalidBucketName", "The specified bucket is not valid.", "/"+bucket)
		return
	}

	// Debug, not Info: an Info line per request is a synchronous log write on
	// the hot path and floods the logs of any busy server.
	slog.Debug("[S3] Request received", "method", r.Method, "path", r.URL.Path, "bucket", bucket, "key", key)

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

// isObjectUpload reports whether the request streams its body into an object
// or part (PutObject, UploadPart), as opposed to sending a configuration or
// command document.
func isObjectUpload(r *http.Request, key string) bool {
	return key != "" && r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") == ""
}

// readVerifiedBody reads a verify-as-you-read body in full, so every
// integrity check has run before the handler sees any of it, and swaps in the
// verified bytes.
func readVerifiedBody(r *http.Request) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxXMLRequestBody+1))
	if err != nil {
		return err
	}
	if len(data) > maxXMLRequestBody {
		return &storage.S3Error{Code: "MaxMessageLengthExceeded", Message: "Your request was too big."}
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(data))
	return nil
}

// isAnonymousReadCandidate reports whether a request is of the only kind a
// public bucket serves without a signature: GET or HEAD of an object's current
// version. It used to be any GET or HEAD of a key, which also exposed ListParts
// (?uploadId=) and every old or deleted version (?versionId=) to anyone.
func isAnonymousReadCandidate(r *http.Request, bucket, key string) bool {
	if bucket == "" || key == "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	q := r.URL.Query()
	return !q.Has("versionId") && !q.Has("uploadId")
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

	headers, matched := evaluateCORS(r, cors)
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
			case <-rt.stopLogs:
				// Take whatever is already queued, then flush and exit.
				for {
					select {
					case task := <-rt.logChan:
						key := task.targetBucket + "\x00" + task.targetPrefix
						blog, exists := buffers[key]
						if !exists {
							blog = &bufferedLog{lastCreated: time.Now().UTC()}
							buffers[key] = blog
						}
						blog.lines = append(blog.lines, task.logLine)
					default:
						flushAll()
						return
					}
				}

			case task := <-rt.logChan:

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

// Shutdown waits for in-flight requests, then flushes buffered access logs.
// It is safe to call more than once; only the first call does any work.
//
// The wait for requests is bounded by ctx. It used to be unconditional, so one
// slow download held server shutdown open however short its timeout. Requests
// still running past the deadline are left to finish on their own; their
// access log entries are dropped rather than written.
func (rt *Router) Shutdown(ctx context.Context) error {
	var err error
	rt.closeOnce.Do(func() {
		if waitErr := rt.activeReqs.waitIdle(ctx); waitErr != nil {
			err = fmt.Errorf("S3 API requests still in flight at the shutdown deadline: %w", waitErr)
		}
		close(rt.stopLogs)
		rt.logWG.Wait()
	})
	return err
}

// inflightCounter counts in-flight requests and lets a caller wait, with a
// deadline, for the count to reach zero.
//
// A sync.WaitGroup can't do this: giving up on Wait leaves it pending, and a
// straggler's Done followed by any later Add then panics ("WaitGroup is
// reused before previous Wait has returned").
type inflightCounter struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n reaches zero; nil while nobody waits
}

func (c *inflightCounter) add() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *inflightCounter) done() {
	c.mu.Lock()
	c.n--
	if c.n == 0 && c.idle != nil {
		close(c.idle)
		c.idle = nil
	}
	c.mu.Unlock()
}

// waitIdle returns once no request is in flight, or with ctx's error if that
// comes first.
func (c *inflightCounter) waitIdle(ctx context.Context) error {
	c.mu.Lock()
	if c.n == 0 {
		c.mu.Unlock()
		return nil
	}
	if c.idle == nil {
		c.idle = make(chan struct{})
	}
	idle := c.idle
	c.mu.Unlock()

	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close is Shutdown with no deadline.
func (rt *Router) Close() {
	_ = rt.Shutdown(context.Background())
}
