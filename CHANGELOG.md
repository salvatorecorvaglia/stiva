# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Breaking

- Replication now mirrors objects to `<STIVA_SYNC_BUCKET>/<source-bucket>/<key>` instead of `<STIVA_SYNC_BUCKET>/<key>`. Every source bucket was previously flattened into the target bucket under the bare object key, so two source buckets holding the same key silently overwrote each other on the replica. Existing mirrors keep their old flat keys and are **not** migrated: re-sync the source, or move the existing objects under the matching per-bucket prefix, before relying on the replica.

### Changed

- `STIVA_DISABLE_MIN_PART_SIZE` is now read through `Config.Load()` like every other setting, instead of a bare `os.Getenv` inside `CompleteMultipartUpload` compared against the exact string `"true"`. It now accepts the same `1`/`yes`/`on` spellings as every other boolean and fails startup validation when set to something unparseable.
- Console API errors no longer echo engine error text, which carries filesystem paths (`failed to create object directory: mkdir /data/buckets/...`). Engine conditions are mapped to an appropriate status with a caller-safe message and the underlying error is logged instead — bringing the console into line with the rule the S3 layer already followed. `ListBuckets`, `HeadBucket` and `GetBucketLocation` on the S3 side did the same thing and are fixed too.
- S3 object responses now set `X-Content-Type-Options: nosniff`.

### Fixed

- `ListObjects` no longer returns an empty page when a prefix is combined with a marker that sorts before it. The seek key was built from `start-after`/`marker` alone and ignored `prefix`, so the cursor landed ahead of the prefix range and the scan stopped on the first non-matching key — reporting no objects at all despite matching keys further on. It now seeks to whichever of the two sorts later.
- Replication no longer silently drops SSE-C encrypted objects after three failed retries. Because the customer key is deliberately never persisted, such an object can never be read back for replication; it is now skipped once with an explicit warning that the mirror will not contain it, instead of scheduling a retry chain that could not succeed and that stalled engine shutdown behind its timers.
- Signed request bodies over 2MiB are no longer left on disk. `HashPayload` spools them to a temp file and replaces `r.Body` with a reader that deletes the file on `Close`, but nothing ever closed it — `net/http` closes the original body, not the replacement — so every such request leaked a `stiva-body-*` file, including requests later rejected for a bad signature. The startup orphan sweep also now reclaims `stiva-chunked-*` files, which it previously never matched.

### Security

- Fixed a stored cross-site scripting vulnerability in the Console file browser. `escapeHtml` round-tripped values through `textContent`/`innerHTML`, which escapes `&`, `<` and `>` but not quotes, and its output is interpolated into double-quoted HTML attributes carrying object keys. An object key containing a double quote could therefore close the attribute and inject an inline event handler, executing script in the Console where the session token is held. Quotes are now escaped as well.
- The S3 `response-content-type` and `response-content-disposition` override parameters are now honoured only on signed requests. On an unauthenticated read from a public bucket they let any stored object be served as arbitrary HTML from the S3 origin, simply by appending a query parameter.
- The Console `Content-Security-Policy` no longer permits inline scripts (`script-src 'self'`). The page ships no inline handlers or inline `<script>` blocks, so `'unsafe-inline'` provided nothing while disabling the protection that would otherwise have contained an injected event handler. `style-src` continues to allow inline styles, which the UI does use.

## [1.2.0] - 2026-08-25

### Added

- `UploadPartCopy` (`PUT .../<key>?partNumber=N&uploadId=ID` with `x-amz-copy-source`) support for multipart uploads, including `x-amz-copy-source-range` and independent source/destination SSE-C parameters.
- `STIVA_MAX_OBJECT_SIZE` configuration to cap the bytes accepted for a single object or multipart part at the storage engine level, independent of SigV4 signing mode (closing a bypass where `UNSIGNED-PAYLOAD` requests skipped the existing payload-size check entirely). Defaults to 5GiB.
- Signature verification for `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` (aws-chunked) request bodies: each chunk is now decoded and its signature validated against the chain seeded by the request's own signature, rather than trusting the framing unverified.
- `stiva_webhook_dropped_total` and `stiva_sync_dropped_total` Prometheus counters, so an operator can detect silent webhook/replication event loss from a full dispatch queue instead of relying solely on log lines.
- `/api/config` Console endpoint exposing non-sensitive server settings (currently the S3 API port) so the UI's top-bar badge reflects the operator's actual `STIVA_S3_PORT` instead of a hardcoded `9000`.
- `?download=1` option on the Console's object presign endpoint, adding a `response-content-disposition: attachment` override for shareable links opened outside the console origin.
- Startup validation of malformed environment variables (e.g. `STIVA_TLS_ENABLED=enabled`, a non-integer `STIVA_S3_PORT`): `Config.Load()` now records values it couldn't parse and `Validate()` fails startup with a diagnostic instead of `Load()` silently substituting a default.

### Changed

- Replication mirroring dispatcher now retries a failed sync attempt up to 3 times with exponential backoff (matching the existing webhook dispatcher behavior), and shutdown waits for any retry chain in flight to finish before returning.
- `ListObjectVersions` rewritten to walk the underlying bbolt cursor directly, one key's versions at a time, instead of loading and sorting every version of every key in the bucket into memory — bounding memory and CPU per page rather than per bucket.
- XML request bodies for bucket subresource/config endpoints (versioning, lifecycle, logging, CORS) are now capped at 2MB via `io.LimitReader`, matching the existing `DeleteObjects` cap, so an unsigned-size payload can't force unbounded `xml.Decoder` allocation.
- Console object downloads now go through an authenticated, same-origin `/objects/download` endpoint instead of a cross-origin presigned S3 URL, since browsers ignore the `<a download>` attribute for cross-origin links.
- `STIVA_JWT_SECRET`, `STIVA_METRICS_TOKEN`, and `STIVA_WEBHOOK_SECRET` are now read once into `Config` and threaded through explicitly, replacing scattered `os.Getenv` reads in `server.go`, `console.go`, and `webhook.go`.
- SSE-C parameter errors in `UploadPart`, `UploadPartCopy`, and `CompleteMultipartUpload` now render as proper SSE-C S3 error responses instead of a generic `InvalidArgument`.
- `Server.Shutdown` is now idempotent, tolerating a signal handler and a manual/orchestrator shutdown call racing each other without panicking.
- The S3 API and Console HTTPS listeners now each get their own `*tls.Config` instance instead of sharing one, avoiding a data race from their independent in-place mutations during concurrent `ServeTLS`.

### Fixed

- `HashPayload` now enforces `STIVA_MAX_OBJECT_SIZE` independently of `Content-Length` for chunked-transfer-encoded request bodies (which report `Content-Length: -1`), closing a gap that let such a request spool an unbounded body to disk.
- `PutObject` now serializes the rename-then-metadata-write sequence per object key, preventing two concurrent PUTs to the same unversioned key from pairing one request's on-disk bytes with the other's ETag/size metadata.
- Startup JWT secret loading no longer silently swallows genuine I/O errors reading the persisted secret, nor silently uses a corrupted/truncated stored secret; both now log and fall back to generating a fresh one.

### Security

- Object and multipart-part uploads are now bounded by `STIVA_MAX_OBJECT_SIZE` at the storage engine layer, closing a bypass where a client sending `UNSIGNED-PAYLOAD` skipped the SigV4 layer's own payload-size enforcement entirely.

## [1.1.0] - 2026-08-12

### Added

- Reorganized project test suite into a standalone `tests/` root directory (`tests/auth`, `tests/config`, `tests/console`, `tests/httpx`, `tests/s3api`, `tests/server`, `tests/storage`).
- Added new integration and fuzz test suites including `paths_fuzz_test.go`, `ssec_multipart_test.go`, `operations_test.go`, `subresource_test.go`, `dbcache_test.go`, `listing_test.go`, `clientip_test.go`, `presign_test.go`, and `validate_test.go`.
- Added `internal/httpx` package providing reliable client IP resolution (`clientip.go`) supporting trusted proxy chains.
- Added decoupled object listing module (`internal/storage/listing.go`) for structured bucket/key listing operations.

### Changed

- Enhanced Admin Web Console accessibility with ARIA attributes, semantic structure, and improved drag-and-drop file upload reliability.
- Upgraded `.golangci.yml` linter configuration to version 2 schema and updated CI linter rules.
- Refactored S3 API router subresource handling, error rendering, and bulk object delete operations.

### Fixed

- Fixed database handle leaks in `TestDeleteBucketVersionedEmptiness` and unclosed server instances in `TestServerStartPortConflict` that caused `unlinkat` access denied errors on Windows CI runners.
- Fixed false-positive integer conversion linting warnings and simplified server shutdown flag assertions.
- Fixed unclosed object readers in filesystem storage tests (`tests/storage/filesystem_test.go`) to prevent file descriptor and resource leaks.

### Security

- Strengthened directory traversal protection across storage engine and multipart upload operations (`internal/storage/filesystem.go`, `internal/storage/multipart.go`, `internal/storage/paths.go`) by enforcing strict path prefix validations against bucket boundaries and multipart temporary directories.

## [1.0.0] - 2026-08-06

### Chore

- Promoted package version to 1.0.0 for initial official Docker Hub release.

## [0.6.0] - 2026-07-27

### Added

- LRU connection cache (bounded capacity) for per-bucket metadata SQLite databases in `MetadataStore` to optimize open file descriptor usage and memory overhead.
- CORS credential header support (`Access-Control-Allow-Credentials: true`) when cross-origin request credentials are enabled.
- Modal dialog keyboard navigation (`ESC` key to dismiss) and rate-limiting error feedback in the Admin Web Console.

### Changed

- Upgraded minimum Go version requirement to Go 1.25 across project dependencies, Dockerfile, CI workflows, and documentation.
- Refactored S3 API router test teardown to perform synchronous log flushing during server shutdown.
- Optimized GitHub Actions CI workflows by enabling Go build caching and smart concurrency cancellation (`cancel-in-progress`).

### Removed

- Redundant `docker-publish` job from the GitHub Actions release workflow.

## [0.5.0] - 2026-07-19

### Added

- Multi-platform Docker image publishing (`linux/amd64`, `linux/arm64`) to GitHub Container Registry (GHCR) upon release tag pushes.
- Concurrency controls (`cancel-in-progress`) to the CI workflow to automatically terminate redundant runs.
- `STIVA_TRUST_PROXY` configuration to support correct client IP extraction from proxy environments using the first address in the `X-Forwarded-For` header.

### Changed

- Centralized request signing logic inside the `internal/auth` package (`auth.SignRequest`), replacing duplicate inline implementations.
- Optimized Prometheus `/metrics` endpoint by caching system disk space query responses for 30 seconds.
- Refactored CORS origin matching to perform domain suffix checks directly against the parsed hostname, fixing wildcard matches on custom port combinations.
- Hardened GitHub Actions release workflow permissions by restricting write permissions to the specific job level.
- Cleaned up duplicate metadata store retrieval logic in `UploadPart` to optimize lock contention on multipart uploads.
- Updated Docker GitHub Actions in release workflow to support Node.js 24 (`setup-qemu-action@v4`, `login-action@v4`, `metadata-action@v6`).

### Fixed

- Expiration rule logic in bucket lifecycle management to correctly calculate object age based on its actual `LastModified` timestamp.

## [0.4.0] - 2026-07-13

### Added

- Aggregated and buffered access logging inside the S3 API router to optimize disk I/O, writing logs in batches (up to 100 entries or every 5 seconds).
- Active request tracking with a `WaitGroup` to ensure graceful shutdown of all outstanding API requests before terminating router log workers.
- Truncated text preview support in the console viewer for files larger than 256 KB using S3 `Range` requests.
- Background silent auto-refresh mechanism (every 10 seconds) for the web console objects list.

### Changed

- Decoupled the monolithic storage engine inside `internal/storage/filesystem.go` into dedicated files: `multipart.go` (multipart uploads), `lifecycle.go` (lifecycle rules/expiration), and `paths.go` (path validation).
- Refactored access log delivery to run on a single background worker thread instead of multiple concurrent workers.
- Streamlined CORS preflight responses to return a forbidden status code (403) upon failure instead of structured S3 errors.
- Updated documentation in `README.md` and `CONTRIBUTING.md` to reflect range requests, metrics, and internal package structure changes.
- Updated `.gitignore` to exclude IDE/editor-specific directories (`.copilot`, `.codex`, `.cagent`).

## [0.3.0] - 2026-07-05

### Added

- `STIVA_METRICS_TOKEN` environment variable configuration to secure the `/metrics` Prometheus endpoint.
- SPA client-side routing wildcard fallback support in the web console, preventing 404 errors on browser page reloads or deep-linked URL paths.
- S3 `GetObject` range request support for compressed/non-seekable streams, dynamically buffering stream chunks and returning `206 Partial Content`.
- Integration tests in `internal/s3api` for non-seekable compressed range request operations.

### Changed

- Consolidated cryptographic signature helpers: moved signature utilities to `internal/auth` (`auth.HmacSHA256`) and reuse them across S3 API verification and mirroring replication.
- Optimized storage database retrieval inside `MetadataStore` to verify bucket registration in the global DB registry prior to instantiating per-bucket connection handlers, preventing automatic creation of deleted/dangling database files on disk.
- Enhanced mirroring replication client by optimizing connection pooling parameters on `http.Transport` to prevent port and socket exhaustion under high loads.
- Adjusted multipart upload completion to strip surrounding quotes from part ETags before validation.
- Fixed S3 `ListObjects` key seek positioning when using delimiters and start-after parameters.

### Security

- Enforced console endpoint and WebSocket security by validating incoming `Origin` headers against the request host and local loopback addresses (`localhost`, `127.0.0.1`).

## [0.2.0] - 2026-06-22

### Added

- `STIVA_S3_ENDPOINT` environment variable configuration to customize public S3 endpoint URLs for console presigned links.
- Skeleton loading shimmers to the Admin Web Console for buckets and objects loading states.
- Concurrency integration tests for MetadataStore `initLocks` reference counting.

### Changed

- Reused `http.Client` for replication mirroring dispatcher (`performSync`) to prevent port/connection exhaustion.
- Optimized metadata database initialization `initLocks` using reference-counted mutexes to prevent memory leak and race conditions on concurrent database lookups.
- Enhanced PDF preview sandboxing in the console iframe to enforce strict script-only permissions (removing `allow-same-origin`).
- Improved webhook dispatcher connection reuse by discarding and closing response bodies.
- Cleaned up unused `logStop` channel in the S3 API router.
- Refactored `readCloserWrapper.Close()` to close underlying closers in LIFO (Last-In-First-Out) order.

## [0.1.0] - 2026-06-15

### Added

- Bucket metadata caching within the metadata store to reduce database lookups.
- Integration of replication sync configurations into the filesystem storage engine.
- Multi-version object deletion support to ensure all historical versions are removed from disk upon object deletion.
- CORS origin wildcard matching with scheme validation (e.g., `https://*.example.com`).
- Multipart upload support for Server-Side Encryption with Customer-provided Keys (SSE-C).
- GitHub Issue and PR templates for standardized community contributions.
- Bounded access log worker pool inside the S3 API Router to queue logs and prevent goroutine explosion under load.
- `STIVA_TRUST_PROXY` environment variable option to respect proxy headers (like `X-Forwarded-For`) for rate limiting console access.
- Startup security warning if default S3 credentials (`stiva` / `stiva123`) are detected.
- Storage engine startup sweep that cleans orphaned temporary/multipart files (`.stiva-tmp-`, etc.) left by previous crashes.
- Webhook and replication mirroring test suites (`webhook_test.go` and `sync_test.go`) covering asynchronous events.

### Changed

- Updated filesystem storage engine initialization in tests to utilize temporary directories and configuration parameters.
- Updated storage engine initialization in console tests with required initialization parameters.
- Simplified image tagging strategy for Docker release publishing to use semver tags and `latest`.
- Updated documentation references to use relative paths/links instead of absolute/hardcoded links.
- Optimized multipart upload performance by reducing lock contention, holding metadata locks only briefly during validation and updates while allowing concurrent disk writes.
- Upgraded GitHub Actions and CI workflow runner environments to use the latest versions.
- Refactored code style, improved error message clarity, and modernized `golangci-lint` configuration settings.
- Removed default credentials from Dockerfile for security hardening.
- Standardized coding guidelines in `CONTRIBUTING.md` regarding constant-time security checks, bounded logging workers, and passive map cleanups.
- Removed loopback IP address (`127.0.0.1`) from auto-generated TLS certificate SAN `DNSNames`.

### Fixed

- Excluded internal system keys (prefixed with `_sys_`) from S3 `ListBuckets` API results to prevent system metadata leakage.
- Deadlock and race conditions in webhook and mirror sync dispatchers by unlocking locks prior to closing queues during shutdown.
- Lock leak in metadata store initialization by ensuring lock cleanup runs via `defer` on errors.
- Flaky integration test assertions in webhook and mirror sync tests by replacing sleep-based waits with polling logic.
- Cleaned up orphaned temporary body files (`stiva-body-*`) in the `tmp` data directory during startup.
- Bucket stripe locking around database scans in `CleanExpiredMultipartUploads` to prevent race conditions during concurrent bucket deletions.
- S3 `DeleteBucketLifecycle` API handler to correctly validate bucket existence before returning `204 No Content`.
- Safely handled GET object errors to avoid potential nil pointer dereference on readers.
- Timing race conditions in the webhook and mirror sync integration tests.

### Removed

- Docker Hub publishing support from the GitHub Actions release workflow.

### Security

- Bumped `github.com/golang-jwt/jwt/v5` from `5.2.1` to `5.2.2`.
- Passive inline garbage collection in the console rate limiter map to prune inactive clients and mitigate memory leak vulnerability.
- Constant-time comparisons (via `crypto/subtle`) for custom SSE-C customer key MD5 comparisons.

## [0.0.1] - 2026-05-27

### Added

- First implementation of Stiva.