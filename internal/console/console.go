package console

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/config"
	"github.com/salvatorecorvaglia/stiva/internal/httpx"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

const (
	errMethodNotAllowed = "method not allowed"
	errInvalidRequest   = "invalid request"
	contentTypeHeader   = "Content-Type"
	errMissingKey       = "missing key parameter"

	// maxJSONRequestBody bounds the JSON body accepted by the console API.
	// These handlers decoded straight from r.Body with no ceiling, unlike their
	// S3-API counterparts, so a caller could force unbounded decoder
	// allocation — on /api/login, without authenticating at all.
	maxJSONRequestBody = 1 << 20 // 1MB
	// maxCompleteParts bounds the parts array accepted when completing a
	// multipart upload, matching the S3 limit on parts per upload.
	maxCompleteParts = storage.MaxPartNumber

	// defaultPresignExpiry is used when the caller does not specify one.
	defaultPresignExpiry = time.Hour
	// defaultMaxPresignExpiry caps generated links. S3 itself allows at most 7
	// days for SigV4 presigned URLs, and these are signed with root credentials.
	defaultMaxPresignExpiry = 7 * 24 * time.Hour
)

//go:embed static/*
var staticFiles embed.FS

type clientRateLimit struct {
	tokens     float64
	lastAccess time.Time
}

type rateLimiter struct {
	mu          sync.Mutex
	clients     map[string]*clientRateLimit
	rate        float64 // tokens per second
	burst       float64
	lastCleanup time.Time
	disabled    bool
}

func newRateLimiter(limitPerMin int) *rateLimiter {
	return &rateLimiter{
		clients:     make(map[string]*clientRateLimit),
		rate:        float64(limitPerMin) / 60.0,
		burst:       float64(limitPerMin),
		lastCleanup: time.Now(),
		// A limit of 0 previously meant "allow nothing", which permanently
		// locked operators out of the console when they set it expecting
		// "unlimited". Disabling is now explicit and 0 is rejected by config
		// validation.
		disabled: config.RateLimitDisabled(limitPerMin),
	}
}

func (rl *rateLimiter) allow(ip string) bool {
	if rl.disabled {
		return true
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()

	// Passive stale client cleanup (older than 10 minutes, runs at most once per minute)
	if now.Sub(rl.lastCleanup) > 1*time.Minute {
		for k, v := range rl.clients {
			if now.Sub(v.lastAccess) > 10*time.Minute {
				delete(rl.clients, k)
			}
		}
		rl.lastCleanup = now
	}

	client, exists := rl.clients[ip]
	if !exists {
		if len(rl.clients) >= 10000 {
			for k, v := range rl.clients {
				if now.Sub(v.lastAccess) > 1*time.Minute {
					delete(rl.clients, k)
				}
			}
		}
		client = &clientRateLimit{
			tokens:     rl.burst,
			lastAccess: now,
		}
		rl.clients[ip] = client
	}

	elapsed := now.Sub(client.lastAccess).Seconds()
	client.tokens += elapsed * rl.rate
	if client.tokens > rl.burst {
		client.tokens = rl.burst
	}
	client.lastAccess = now

	if client.tokens >= 1 {
		client.tokens -= 1
		return true
	}
	return false
}

// getClientIP resolves the rate-limiting key for a request.
//
// Proxy trust now comes from the handler's configuration rather than a
// package-level env lookup, so the console and the S3 API can no longer
// disagree about whether X-Forwarded-For is trustworthy.
func (h *Handler) getClientIP(r *http.Request) string {
	return httpx.ClientIP(r, h.trustProxy, h.trustedProxyHops)
}

// Options configures a console Handler. It replaces a seven-argument
// constructor in which the proxy-trust setting was not threaded through at all.
type Options struct {
	Engine           storage.Engine
	Creds            *auth.Credentials
	S3Port           int
	Region           string
	S3Endpoint       string
	LoginRateLimit   int
	APIRateLimit     int
	TrustProxy       bool
	TrustedProxyHops int
	// MaxPresignExpiry bounds how far in the future a generated presigned URL
	// may be valid. Zero selects defaultMaxPresignExpiry.
	MaxPresignExpiry time.Duration
	// MetricsToken, if set, is the bearer token required to read /metrics
	// independent of a console session. Empty means only a valid console
	// session token is accepted.
	MetricsToken string
}

// Handler serves the web console UI and its REST API.
type Handler struct {
	engine           storage.Engine
	creds            *auth.Credentials
	s3Port           int
	region           string
	s3Endpoint       string
	mux              *http.ServeMux
	loginLimiter     *rateLimiter
	apiLimiter       *rateLimiter
	trustProxy       bool
	trustedProxyHops int
	maxPresignExpiry time.Duration
	metricsToken     string
}

// NewHandler creates a new console handler.
func NewHandler(opts Options) *Handler {
	hops := opts.TrustedProxyHops
	if hops < 1 {
		hops = 1
	}
	maxExpiry := opts.MaxPresignExpiry
	if maxExpiry <= 0 {
		maxExpiry = defaultMaxPresignExpiry
	}

	h := &Handler{
		engine:           opts.Engine,
		creds:            opts.Creds,
		s3Port:           opts.S3Port,
		region:           opts.Region,
		s3Endpoint:       opts.S3Endpoint,
		mux:              http.NewServeMux(),
		loginLimiter:     newRateLimiter(opts.LoginRateLimit),
		apiLimiter:       newRateLimiter(opts.APIRateLimit),
		trustProxy:       opts.TrustProxy,
		trustedProxyHops: hops,
		maxPresignExpiry: maxExpiry,
		metricsToken:     opts.MetricsToken,
	}
	h.setupRoutes()
	return h
}

func (h *Handler) rateLimitMiddleware(limiter *rateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !limiter.allow(h.getClientIP(r)) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many requests"})
			return
		}
		next(w, r)
	}
}

func (h *Handler) setupRoutes() {
	// API routes
	h.mux.HandleFunc("/api/login", h.rateLimitMiddleware(h.loginLimiter, h.handleLogin))
	h.mux.HandleFunc("/api/config", h.rateLimitMiddleware(h.apiLimiter, h.authMiddleware(h.handleGetConfig)))
	h.mux.HandleFunc("/api/buckets", h.rateLimitMiddleware(h.apiLimiter, h.authMiddleware(h.handleBuckets)))
	h.mux.HandleFunc("/api/buckets/", h.rateLimitMiddleware(h.apiLimiter, h.authMiddleware(h.handleBucketObjects)))

	// Prometheus metrics endpoint
	h.mux.HandleFunc("/metrics", h.handleMetrics)

	// Static files (embedded frontend) with SPA wildcard fallback
	staticFS, _ := fs.Sub(staticFiles, "static")
	fileServer := http.FileServer(http.FS(staticFS))
	h.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			fileServer.ServeHTTP(w, r)
			return
		}

		// If the file exists in staticFS, serve it
		f, err := staticFS.Open(path)
		if err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}

		// If requesting a file path (contains extension) that doesn't exist, return 404
		if strings.Contains(path, ".") {
			http.NotFound(w, r)
			return
		}

		// Otherwise, serve index.html for client-side SPA routing
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}

// securityHeaders applies the baseline browser protections the console was
// serving without: framing, MIME sniffing, referrer leakage and a CSP.
//
// The CSP permits inline *styles* because the SPA sets style attributes, both
// in index.html and on the nodes app.js builds. It deliberately does not
// permit inline scripts: index.html carries no inline handlers and no inline
// <script>, only <script src="app.js">, so 'unsafe-inline' bought nothing
// there while disarming the main protection a script-src offers — under it,
// an injected on*= attribute (say, from an object key rendered into the file
// browser) would still execute. 'frame-ancestors none' is the modern
// equivalent of DENY.
func securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy",
		"default-src 'self'; "+
			"img-src 'self' data: blob:; "+
			"media-src 'self' blob:; "+
			"style-src 'self' 'unsafe-inline'; "+
			"script-src 'self'; "+
			"connect-src 'self'; "+
			"font-src 'self'; "+
			"frame-src 'self' blob:; "+
			"object-src 'none'; "+
			"base-uri 'none'; "+
			"form-action 'self'; "+
			"frame-ancestors 'none'")
}

// ServeHTTP implements the http.Handler interface.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	securityHeaders(w)

	// Set CORS headers for console
	// CORS: Only allow requests from the same origin.
	// In production, this should be set to the actual console URL.
	origin := r.Header.Get("Origin")
	if origin != "" {
		if isValidConsoleOrigin(origin, r.Host) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		} else {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "unauthorized origin"})
			return
		}
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Max-Age", "3600")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	h.mux.ServeHTTP(w, r)
}

// authMiddleware wraps a handler with JWT authentication.
func (h *Handler) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		token := strings.TrimPrefix(authHeader, "Bearer ")
		_, err := ValidateToken(token)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			return
		}

		next(w, r)
	}
}

// handleGetConfig exposes non-sensitive server settings the UI needs to
// render itself correctly — currently just the S3 API port, which the
// top-bar badge used to hardcode as "Port 9000" regardless of the operator's
// actual STIVA_S3_PORT.
func (h *Handler) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": errMethodNotAllowed})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"s3Port": h.s3Port})
}

// handleLogin handles POST /api/login.
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": errMethodNotAllowed})
		return
	}

	var req struct {
		AccessKey string `json:"accessKey"`
		SecretKey string `json:"secretKey"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxJSONRequestBody)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidRequest})
		return
	}

	// Constant-time comparison: a plain != leaks the secret's prefix length
	// through response timing, one byte at a time.
	accessOK := subtle.ConstantTimeCompare([]byte(req.AccessKey), []byte(h.creds.AccessKey)) == 1
	secretOK := subtle.ConstantTimeCompare([]byte(req.SecretKey), []byte(h.creds.SecretKey)) == 1
	if !accessOK || !secretOK {
		slog.Warn("[Console] Failed login attempt", "ip", h.getClientIP(r))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}

	token, err := GenerateToken(req.AccessKey)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate token"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// handleBuckets handles /api/buckets (GET = list, POST = create).
func (h *Handler) handleBuckets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listBuckets(w, r)
	case http.MethodPost:
		h.createBucket(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (h *Handler) listBuckets(w http.ResponseWriter, _ *http.Request) {
	buckets, err := h.engine.ListBuckets()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	type bucketResp struct {
		Name         string `json:"name"`
		CreationDate string `json:"creationDate"`
		ObjectCount  int    `json:"objectCount"`
		IsPublic     bool   `json:"isPublic"`
	}

	var result []bucketResp
	for _, b := range buckets {
		count, _ := h.engine.CountObjects(b.Name)
		result = append(result, bucketResp{
			Name:         b.Name,
			CreationDate: b.CreationDate.UTC().Format("2006-01-02T15:04:05.000Z"),
			ObjectCount:  count,
			IsPublic:     b.IsPublic,
		})
	}

	if result == nil {
		result = []bucketResp{}
	}

	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) createBucket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxJSONRequestBody)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}

	// Validate bucket name (SEC-7)
	if !storage.IsValidBucketName(req.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bucket name: must be 3-63 characters, lowercase letters, numbers, hyphens and periods"})
		return
	}

	if err := h.engine.CreateBucket(req.Name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name})
}

// handleBucketObjects handles /api/buckets/<name>/... routes.
func (h *Handler) handleBucketObjects(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/buckets/")
	parts := strings.SplitN(path, "/", 2)
	bucketName := parts[0]

	// Validate bucket name (SEC-7 / Traversal Protection)
	if !storage.IsValidBucketName(bucketName) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bucket name"})
		return
	}

	// Check if this is a bucket delete or object operations
	if len(parts) == 1 || parts[1] == "" {
		if r.Method == http.MethodDelete {
			h.deleteBucket(w, r, bucketName)
			return
		}
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	action := parts[1]

	switch {
	case action == "public" && r.Method == http.MethodPost:
		h.handleSetBucketPublic(w, r, bucketName)
	case action == "public" && r.Method == http.MethodGet:
		h.handleGetBucketPublic(w, r, bucketName)
	case action == "objects" && r.Method == http.MethodGet:
		h.listObjects(w, r, bucketName)
	case action == "objects/presign" && r.Method == http.MethodGet:
		h.handlePresignObject(w, r, bucketName)
	case action == "objects/upload" && r.Method == http.MethodPost:
		h.uploadObject(w, r, bucketName)
	case action == "objects/download" && r.Method == http.MethodGet:
		h.downloadObject(w, r, bucketName)
	case action == "objects" && r.Method == http.MethodDelete:
		h.deleteObject(w, r, bucketName)
	case action == "multipart/initiate" && r.Method == http.MethodPost:
		h.initiateMultipart(w, r, bucketName)
	case action == "multipart/upload-part" && r.Method == http.MethodPost:
		h.uploadPart(w, r, bucketName)
	case action == "multipart/complete" && r.Method == http.MethodPost:
		h.completeMultipart(w, r, bucketName)
	case action == "multipart/abort" && r.Method == http.MethodPost:
		h.abortMultipart(w, r, bucketName)
	case action == "lifecycle" && r.Method == http.MethodGet:
		h.handleGetLifecycle(w, r, bucketName)
	case action == "lifecycle" && r.Method == http.MethodPost:
		h.handlePutLifecycle(w, r, bucketName)
	case action == "lifecycle" && r.Method == http.MethodDelete:
		h.handleDeleteLifecycle(w, r, bucketName)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (h *Handler) deleteBucket(w http.ResponseWriter, _ *http.Request, name string) {
	if err := h.engine.DeleteBucket(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
}

func (h *Handler) handleSetBucketPublic(w http.ResponseWriter, r *http.Request, bucket string) {
	publicStr := r.URL.Query().Get("public")
	public := publicStr == "true"

	if err := h.engine.SetBucketPublic(bucket, public); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"bucket": bucket, "public": public})
}

func (h *Handler) handleGetBucketPublic(w http.ResponseWriter, _ *http.Request, bucket string) {
	public, err := h.engine.IsBucketPublic(bucket)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"public": public})
}

func (h *Handler) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	prefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	if delimiter == "" {
		delimiter = "/"
	}

	// Clamped to the same ceiling the S3 API applies. This used to accept any
	// positive value, so a single console request could ask the engine to
	// accumulate an unbounded number of objects in memory.
	maxKeys := httpx.MaxKeys(r.URL.Query().Get("maxKeys"), httpx.MaxPageSize)

	continuationToken := r.URL.Query().Get("continuationToken")

	effectivePrefix := prefix
	effectiveDelimiter := delimiter
	if searchPrefix := r.URL.Query().Get("searchPrefix"); searchPrefix != "" {
		effectivePrefix = prefix + searchPrefix
		effectiveDelimiter = "" // Recursive search
	}

	output, err := h.engine.ListObjects(&storage.ListObjectsInput{
		Bucket:            bucket,
		Prefix:            effectivePrefix,
		Delimiter:         effectiveDelimiter,
		MaxKeys:           maxKeys,
		ContinuationToken: continuationToken,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	type objectResp struct {
		Key          string `json:"key"`
		Size         int64  `json:"size"`
		LastModified string `json:"lastModified"`
		ETag         string `json:"etag"`
		IsPrefix     bool   `json:"isPrefix"`
	}

	var items []objectResp

	// Add common prefixes (folders)
	for _, p := range output.CommonPrefixes {
		items = append(items, objectResp{
			Key:      p,
			IsPrefix: true,
		})
	}

	// Add objects
	for _, obj := range output.Objects {
		items = append(items, objectResp{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         obj.ETag,
			IsPrefix:     false,
		})
	}

	if items == nil {
		items = []objectResp{}
	}

	type listObjectsResp struct {
		Items                 []objectResp `json:"items"`
		IsTruncated           bool         `json:"isTruncated"`
		NextContinuationToken string       `json:"nextContinuationToken,omitempty"`
	}

	writeJSON(w, http.StatusOK, listObjectsResp{
		Items:                 items,
		IsTruncated:           output.IsTruncated,
		NextContinuationToken: output.NextContinuationToken,
	})
}

func (h *Handler) uploadObject(w http.ResponseWriter, r *http.Request, bucket string) {
	// Stream the multipart body part-by-part instead of calling
	// ParseMultipartForm, which spooled the entire upload into the *OS* temp
	// directory (ignoring STIVA_DATA_DIR) before a single byte reached storage.
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to parse upload"})
		return
	}

	// Prefer the key from the query string. Streaming means form fields are only
	// visible if they precede the file part, so relying on field order alone
	// would silently fall back to the bare filename and flatten folder uploads.
	var (
		key         = r.URL.Query().Get("key")
		filename    string
		contentType string
		info        *storage.ObjectInfo
	)

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to parse upload"})
			return
		}

		switch part.FormName() {
		case "key":
			// Form fields are small; bound them anyway so a hostile client
			// cannot stream an unbounded "key".
			buf, err := io.ReadAll(io.LimitReader(part, 4096))
			part.Close()
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidRequest})
				return
			}
			if key == "" {
				key = string(buf)
			}

		case "file":
			filename = part.FileName()
			contentType = part.Header.Get(contentTypeHeader)
			if contentType == "" {
				contentType = "application/octet-stream"
			}

			objectKey := key
			if objectKey == "" {
				objectKey = filename
			}
			if objectKey == "" {
				part.Close()
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing key"})
				return
			}

			// Size is unknown when streaming, so pass -1 and let the engine
			// record whatever was actually written.
			info, err = h.engine.PutObject(r.Context(), bucket, objectKey, part, -1, contentType)
			part.Close()
			if err != nil {
				slog.Error("[Console] Upload error", "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "upload failed"})
				return
			}

		default:
			part.Close()
		}
	}

	if info == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no file provided"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key":  info.Key,
		"size": info.Size,
		"etag": info.ETag,
	})
}

func (h *Handler) downloadObject(w http.ResponseWriter, r *http.Request, bucket string) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMissingKey})
		return
	}

	reader, info, err := h.engine.GetObject(r.Context(), bucket, key, "")
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "object not found"})
		return
	}
	defer reader.Close()

	// Extract filename from key and sanitize for Content-Disposition header (SEC-6)
	parts := strings.Split(key, "/")
	filename := parts[len(parts)-1]

	w.Header().Set(contentTypeHeader, info.ContentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	// Content is user-supplied: never let the browser sniff it into something
	// executable, and never render it inline in this origin.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, reader)
}

func (h *Handler) deleteObject(w http.ResponseWriter, r *http.Request, bucket string) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMissingKey})
		return
	}

	if _, _, err := h.engine.DeleteObject(bucket, key, ""); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"deleted": key})
}

// writeStorageError renders an engine error for a console caller.
//
// Engine errors routinely carry filesystem paths ("failed to create object
// directory: mkdir /data/buckets/..."), and the console passed err.Error()
// straight through on every mutating endpoint. The S3 side is explicit that
// internal error text must never reach a caller; this brings the console into
// line. A *storage.S3Error describes a caller-facing condition and its message
// is safe to return; anything else is logged and replaced with fallback.
func writeStorageError(w http.ResponseWriter, err error, fallback string) {
	var s3Err *storage.S3Error
	if errors.As(err, &s3Err) {
		status := http.StatusBadRequest
		switch s3Err.Code {
		case "NoSuchBucket", "NoSuchKey", "NoSuchUpload", "NoSuchLifecycleConfiguration":
			status = http.StatusNotFound
		case "BucketAlreadyOwnedByYou", "BucketAlreadyExists", "BucketNotEmpty":
			status = http.StatusConflict
		case "EntityTooLarge":
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, status, map[string]string{"error": s3Err.Message})
		return
	}
	slog.Error("[Console] Internal error", "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fallback})
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set(contentTypeHeader, "application/json")
	// API responses describe account state and must not be cached by browsers
	// or intermediaries.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (h *Handler) initiateMultipart(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		Key         string `json:"key"`
		ContentType string `json:"contentType"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxJSONRequestBody)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidRequest})
		return
	}
	if req.Key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing key"})
		return
	}
	if req.ContentType == "" {
		req.ContentType = "application/octet-stream"
	}

	info, err := h.engine.CreateMultipartUpload(bucket, req.Key, req.ContentType)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"uploadId": info.UploadID,
		"key":      info.Key,
	})
}

// uploadPart streams a single multipart chunk to the engine.
//
// Like uploadObject, it reads the multipart body part-by-part rather than
// calling ParseMultipartForm, which spooled the whole chunk into the *OS* temp
// directory — ignoring STIVA_DATA_DIR — before any of it reached storage. That
// fix was applied to uploadObject but not here, even though parts are the
// larger of the two by design.
//
// Streaming means the metadata fields are only visible if they precede the file
// part. The console sends them in that order; a client that does not is told so
// explicitly rather than having its upload silently misfiled.
func (h *Handler) uploadPart(w http.ResponseWriter, r *http.Request, bucket string) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to parse upload"})
		return
	}

	var (
		uploadID   = r.URL.Query().Get("uploadId")
		key        = r.URL.Query().Get("key")
		partNumber int
		partInfo   *storage.PartInfo
	)
	if v := r.URL.Query().Get("partNumber"); v != "" {
		partNumber, _ = strconv.Atoi(v)
	}

	readField := func(part *multipart.Part) (string, error) {
		buf, err := io.ReadAll(io.LimitReader(part, 4096))
		part.Close()
		return string(buf), err
	}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to parse upload"})
			return
		}

		switch part.FormName() {
		case "uploadId":
			v, err := readField(part)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidRequest})
				return
			}
			if uploadID == "" {
				uploadID = v
			}

		case "key":
			v, err := readField(part)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidRequest})
				return
			}
			if key == "" {
				key = v
			}

		case "partNumber":
			v, err := readField(part)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidRequest})
				return
			}
			if partNumber == 0 {
				partNumber, _ = strconv.Atoi(v)
			}

		case "file":
			if partNumber < 1 || partNumber > storage.MaxPartNumber {
				part.Close()
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": fmt.Sprintf("part number must be between 1 and %d, and must be sent before the file part", storage.MaxPartNumber),
				})
				return
			}
			if uploadID == "" || key == "" {
				part.Close()
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "uploadId and key must be sent before the file part",
				})
				return
			}

			// Size is unknown when streaming, so pass -1 and let the engine
			// record whatever was actually written.
			partInfo, err = h.engine.UploadPart(r.Context(), bucket, key, uploadID, partNumber, part, -1)
			part.Close()
			if err != nil {
				writeStorageError(w, err, "failed to upload part")
				return
			}

		default:
			part.Close()
		}
	}

	if partInfo == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no file chunk provided"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"etag":       partInfo.ETag,
		"partNumber": partInfo.PartNumber,
	})
}

func (h *Handler) completeMultipart(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		UploadID string `json:"uploadId"`
		Key      string `json:"key"`
		Parts    []struct {
			PartNumber int    `json:"partNumber"`
			ETag       string `json:"etag"`
		} `json:"parts"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxJSONRequestBody)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidRequest})
		return
	}

	if len(req.Parts) > maxCompleteParts {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("too many parts: at most %d are allowed", maxCompleteParts),
		})
		return
	}

	var s3Parts []storage.CompletePart
	for _, p := range req.Parts {
		s3Parts = append(s3Parts, storage.CompletePart{
			PartNumber: p.PartNumber,
			ETag:       p.ETag,
		})
	}

	info, err := h.engine.CompleteMultipartUpload(r.Context(), bucket, req.Key, req.UploadID, s3Parts)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"key":  info.Key,
		"size": info.Size,
		"etag": info.ETag,
	})
}

func (h *Handler) abortMultipart(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		UploadID string `json:"uploadId"`
		Key      string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxJSONRequestBody)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errInvalidRequest})
		return
	}

	err := h.engine.AbortMultipartUpload(bucket, req.Key, req.UploadID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
}

func (h *Handler) handlePresignObject(w http.ResponseWriter, r *http.Request, bucket string) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMissingKey})
		return
	}

	// Presigned URLs are signed with the root credentials, so an unbounded
	// expiry effectively hands out a permanent root-signed link. Clamp it.
	expires := defaultPresignExpiry
	if expiresStr := r.URL.Query().Get("expires"); expiresStr != "" {
		val, err := strconv.Atoi(expiresStr)
		if err != nil || val <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expires must be a positive number of seconds"})
			return
		}
		expires = time.Duration(val) * time.Second
	}
	if expires > h.maxPresignExpiry {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("expires must not exceed %d seconds", int(h.maxPresignExpiry.Seconds())),
		})
		return
	}

	s3Endpoint := h.s3Endpoint
	if s3Endpoint == "" {
		// Resolve the S3 host from the incoming request (replacing port)
		scheme := "http"
		if r.TLS != nil || (h.trustProxy && r.Header.Get("X-Forwarded-Proto") == "https") {
			scheme = "https"
		}

		hostWithoutPort := r.Host
		if strings.Contains(r.Host, ":") {
			h, _, err := net.SplitHostPort(r.Host)
			if err == nil {
				hostWithoutPort = h
			}
		}

		// Build the S3 endpoint URL
		s3Endpoint = fmt.Sprintf("%s://%s:%d", scheme, hostWithoutPort, h.s3Port)
	}

	// Recipients open a share link directly, outside the console origin, so
	// there's no <a download> to fall back on and the object would otherwise
	// just render inline per its stored Content-Type. Opt-in via ?download=1
	// so other callers of this endpoint aren't forced into that behavior.
	extraParams := url.Values{}
	if r.URL.Query().Get("download") == "1" {
		filename := key
		if idx := strings.LastIndex(key, "/"); idx != -1 {
			filename = key[idx+1:]
		}
		extraParams.Set("response-content-disposition",
			mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	}

	presignedURL, err := auth.PresignGetObject(
		h.creds.AccessKey,
		h.creds.SecretKey,
		h.region,
		bucket,
		key,
		expires,
		s3Endpoint,
		extraParams,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"url": presignedURL})
}

func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	authorized := false

	authHeader := r.Header.Get("Authorization")
	if h.metricsToken != "" {
		expected := "Bearer " + h.metricsToken
		if subtle.ConstantTimeCompare([]byte(authHeader), []byte(expected)) == 1 {
			authorized = true
		}
	}

	if !authorized && authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if _, err := ValidateToken(token); err == nil {
			authorized = true
		}
	}

	if !authorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="Metrics"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	dataDir := "."
	if fsEng, ok := h.engine.(interface{ DataDir() string }); ok {
		dataDir = fsEng.DataDir()
	}

	metricsStr := storage.GlobalMetrics.FormatPrometheus(dataDir)
	w.Header().Set(contentTypeHeader, "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(metricsStr))
}

func isValidConsoleOrigin(origin string, requestHost string) bool {
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if strings.EqualFold(u.Host, requestHost) {
		return true
	}
	if u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" {
		return true
	}
	return false
}

func (h *Handler) handleGetLifecycle(w http.ResponseWriter, _ *http.Request, bucket string) {
	lc, err := h.engine.GetBucketLifecycle(bucket)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, lc)
}

func (h *Handler) handlePutLifecycle(w http.ResponseWriter, r *http.Request, bucket string) {
	var req storage.LifecycleConfiguration
	if err := json.NewDecoder(io.LimitReader(r.Body, maxJSONRequestBody)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	if err := h.engine.PutBucketLifecycle(bucket, &req); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "lifecycle configuration updated"})
}

func (h *Handler) handleDeleteLifecycle(w http.ResponseWriter, _ *http.Request, bucket string) {
	if err := h.engine.DeleteBucketLifecycle(bucket); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "lifecycle configuration deleted"})
}
