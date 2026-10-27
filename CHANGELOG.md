# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Add controlled Compose image updates with health checks, a persistent image journal, and recovery to previous images.

### Changed

- Remove the pull-request Dependency Review job from CI.
- Update Biome to `2.6.3` and refresh console branding.

### Security

- Confine blob directory creation, legacy null-version blob writes and cleanup, and parent-directory synchronization to the data root using `os.Root`, rejecting invalid bucket names and symlink escapes.

## [1.2.0] - 2026-10-26

### Added

- Console bucket lifecycle controls to add prefix-based object expiration rules, enable or disable rules, and remove rules while preserving existing lifecycle actions.
- Console bucket versioning controls and paginated, prefix-filtered history with object versions, delete markers and version-specific downloads.
- Console storage overview and per-bucket usage with logical current and previous version sizes, object and version counts, delete markers and available disk space, backed by cached snapshots refreshed every minute.
- Offline `backup`, `verify-backup` and `restore` commands with file checksums, bbolt integrity checks, a writer lock and staged publication.
- Production Compose with HTTPS reverse proxy, private console, digest validation, independent secrets and bounded logs.
- Durable replication journal committed with object metadata, startup backfill, retry/reconciliation and pending/failure metrics.
- Storage readiness probes, request duration histograms, inode metrics and Prometheus alert rules.
- Container qualification scripts for AWS CLI compatibility, concurrent transfers, SIGKILL recovery, ENOSPC recovery and backup restoration.
- Production operations, backup scheduling and rollback instructions.
- Biome linting and formatting for console JavaScript and CSS, with console lint and test checks in CI.
- CI workflow validation with actionlint, plus manual, merge queue and weekly scheduled CI runs.

### Changed

- Flush object/part data and directory entries before publishing metadata; use immutable multipart part files while keeping legacy parts readable.
- Copy legacy object data into durable blobs before promoting null versions; defer obsolete object cleanup until metadata commits.
- Require Go 1.26.10, pin container base images, and gate tag releases on the full CI workflow with container smoke tests.
- Produce image provenance, SBOMs and immutable digest summaries for releases.
- Use `corvaglia91/stiva:latest` in Docker examples and the default Compose configuration.
- Update `golang.org/x/sys` to v0.49.0 and Biome to v2.6.2.
- Pin GitHub Actions to commit hashes, disable persisted checkout credentials, and bound workflow jobs and tests with timeouts.
- Expand CI builds to Linux ARM64 and macOS AMD64, and pin Windows tests to Windows Server 2025.
- Require valid release versions and matching, nonempty changelog entries before publishing releases.

### Fixed

- Allow explicit null-version reads of objects written before versioning was enabled, before their metadata is promoted to a version record.
- Prevent path and symlink traversal outside the storage root when promoting legacy null versions.
- Keep prerelease Docker images from updating the `latest` tag.
- Fail CI when total coverage is missing or invalid, or the coverage artifact is absent.

## [1.1.0] - 2026-10-15

### Added

- Configurable incomplete multipart upload expiry through `STIVA_MULTIPART_EXPIRY` (default: seven days; minimum: one hour).
- Console API origin allowlist through `STIVA_CONSOLE_ALLOWED_ORIGINS` for reverse proxies and local frontend development.
- Console folder uploads with preserved directory structure, upload cancellation, recursive folder deletion, and batch object deletion.
- Short-lived, single-use console download links that let browsers stream large files without buffering them in memory.
- Unauthenticated `/_health` endpoints on both the S3 API and console servers.
- Verification of upload `Content-MD5` and S3 checksum headers, plus signed and unsigned AWS streaming checksum trailers.

### Changed

- Store new object data in independent blob files, supporting overlapping keys such as `a` and `a/b`, folder markers, and keys differing only in case. Existing objects remain readable in the legacy layout and move to blobs when overwritten.
- Stream large signed and AWS chunked uploads through integrity verification instead of spooling the entire payload to temporary files.
- Use file-backed downloads for uncompressed, unencrypted objects to enable sendfile transfers.
- Run multipart and lifecycle cleanup at startup and hourly, and bound replication and webhook shutdown by the shutdown deadline.
- Update release workflow runners to Ubuntu 24.04.

### Fixed

- Require SigV4 signatures to cover the Host header and correctly encode spaces in presigned URL query parameters.
- Invalidate console sessions after access or secret key rotation and isolate session signing state per handler.
- Correct object and version listing pagination, including delimiter prefixes and deleted version markers.
- Preserve objects written before versioning was enabled as null versions, handle suspended versioning correctly, and keep replication consistent when deleting versions.
- Coordinate concurrent bucket creation, object commits, and deletion to prevent missing data and inconsistent metadata.
- Validate lifecycle filters and reject unsupported conditions instead of expiring more objects than intended.
- Correct encrypted range seeking, repeated pooled gzip reader closure, and multipart upload metrics after restart.
- Improve S3 conditional request handling, SSE-C response headers, and propagation of upload integrity and storage errors.
- Show actionable console upload and download errors and distinguish invalid login credentials from expired sessions.
- Keep Docker Compose data in the mounted `/data` volume and probe both servers over HTTP or HTTPS for container health.
- Report listener failures and wait for background cleanup before closing storage during shutdown.

## [1.0.0] - 2026-09-23

### Added

- First implementation of Stiva.
