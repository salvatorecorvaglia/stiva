# Stiva 🖼️

**Self-hosted, high-performance S3-compatible object storage server with web console**

**Stiva** is a high-performance object storage server written in Go that implements the Amazon S3 API. It's designed for self-hosted environments where you need S3 compatibility without the complexity of a full distributed system.

---

## ✨ Features

### 📡 S3 API Compatibility
*   **Core Bucket & Object Operations**: Full support for listing, creating, and deleting buckets, as well as putting, getting, and deleting objects.
*   **Multipart Uploads**: High-performance concurrent multipart upload support, allowing you to upload large files in chunks with optimized lock contention, including server-side part copying (`UploadPartCopy`) from existing objects.
*   **Bucket Lifecycle Rules**: Automatic object expiration rules based on actual object age (`LastModified` timestamp checks).
*   **Virtual-Host Routing**: Seamless virtual-host bucket resolution support based on custom base domain suffixes.
*   **CORS (Cross-Origin Resource Sharing)**: Highly configurable CORS handlers supporting exact/wildcard domain matches with scheme, port, and credential header validation.
*   **SSE-C Encryption**: Server-Side Encryption with Customer-provided keys utilizing constant-time cryptographic checks.
*   **Partial Content / Range Requests**: Seekable `GetObject` range requests supporting chunk buffering for compressed streams.
*   **Streaming SigV4 Payloads**: Full verification of `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` (aws-chunked) uploads, validating each chunk's signature against the chain seeded by the request signature.
*   **Configurable Upload Size Limits**: Optional `STIVA_MAX_OBJECT_SIZE` cap enforced at the storage engine level, closing signing-mode bypasses (e.g. `UNSIGNED-PAYLOAD`).

### 🖥️ Built-In Web Admin Console
*   **Stunning Dashboard UI**: Modern SPA dashboard built with vanilla HTML, CSS, and JS (zero heavy npm builds required) featuring rate-limit feedback, ARIA accessibility markup, and ESC key modal closing shortcuts.
*   **Bucket Management**: Create and delete buckets directly from the UI.
*   **Object Browser**: Interactive object navigation, supporting folder-like path hierarchies.
*   **Drag & Drop Uploads**: Fast, intuitive file uploading directly to your S3 storage with progress tracking.
*   **Rich Previews**: Instant browser previews for text, images, and sandboxed PDF files (with strict script-only iframe boundaries).
*   **Presigned Links**: Generate temporary, shareable download links for objects with custom expiry windows.

### 🔄 Replication & Event Notifications
*   **Active-Passive Mirroring**: Asynchronous replication dispatcher that automatically mirrors newly uploaded objects to a remote S3-compatible destination.
*   **Webhook Events**: Lightweight JSON payload webhook dispatcher that POSTs notifications to target endpoints on object creation or deletion, with optional HMAC-SHA256 payload signing (`STIVA_WEBHOOK_SECRET`) and retries with exponential backoff.
*   **Prometheus Metrics**: High-performance `/metrics` endpoint presenting native storage metrics — including dropped webhook/replication event counters — secured with a custom token or Console session JWT.

### 🛡️ Production Hardening & Reliability
*   **Structured Logging**: Production-grade JSON or text structured logs via Go's native `log/slog`.
*   **LRU Database Caching**: Bounded LRU caching for per-bucket SQLite metadata connection handles to optimize file descriptor usage and memory footprint under multi-bucket workloads.
*   **Access Log Buffering**: High-throughput access log worker that batches disk writes (up to 100 entries or every 5s) to reduce lock contention and I/O.
*   **Graceful Shutdown**: Monitors shutdown signals (`SIGINT`/`SIGTERM`) and tracks active requests using a `sync.WaitGroup` to ensure no connection is dropped mid-flight.
*   **Orphan Cleanups**: Automated startup sweep that identifies and deletes partial multipart/temporary files left over by unexpected system crashes.

---

## 🚀 Getting Started

### Prerequisites
- **Go 1.26 or higher** (to compile and run locally)
- **Docker** and **Docker Compose** (for containerized deployments)

---

### Method 1: Using Docker Compose (Recommended)

Stiva is distributed as a multi-arch container image. You can spin up Stiva with a persistent volume using `docker-compose.yml`:

1. Copy the environment variables:
   ```bash
   cp .env.example .env
   ```
2. Run Docker Compose:
   ```bash
   docker compose up -d
   ```
3. Access the services:
   *   **S3 API**: `http://localhost:9000`
   *   **Web Console**: `http://localhost:9001` (Default login credentials: Access Key: `stiva` / Secret Key: `stiva123`)

---

### Method 2: Running with Docker CLI

Alternatively, run the official pre-built image from Docker Hub:

```bash
docker run -d \
  -p 9000:9000 \
  -p 9001:9001 \
  -v $(pwd)/data:/data \
  -e STIVA_ACCESS_KEY=stiva \
  -e STIVA_SECRET_KEY=stiva123 \
  salvatorecorvaglia/stiva:latest
```

To build and run the image locally instead:

```bash
# Build the Docker image locally
docker build -t stiva .

# Run the local container
docker run -d \
  -p 9000:9000 \
  -p 9001:9001 \
  -v $(pwd)/data:/data \
  -e STIVA_ACCESS_KEY=stiva \
  -e STIVA_SECRET_KEY=stiva123 \
  stiva
```

---

### Method 3: Building from Source

1. Clone the repository:
   ```bash
   git clone https://github.com/salvatorecorvaglia/stiva.git
   cd stiva
   ```
2. Configure your environment variables:
   ```bash
   cp .env.example .env
   ```
3. Build the binary:
   ```bash
   go build -ldflags="-w -s" -o stiva ./cmd/stiva
   ```
4. Run Stiva with environment configuration loaded:
   ```bash
   export $(grep -v '^#' .env | xargs) && ./stiva
   ```

---

## ⚙️ Configuration Reference

Stiva is configured exclusively via environment variables. You can find a baseline configuration file template in [.env.example](.env.example).

| Environment Variable | Default Value | Description |
| :--- | :--- | :--- |
| **`STIVA_ACCESS_KEY`** | `stiva` | Access key used for S3 client credentials and Web Console login. |
| **`STIVA_SECRET_KEY`** | `stiva123` | Secret key used for S3 client credentials and Web Console login. |
| **`STIVA_DATA_DIR`** | `/data` (Docker) / `./data` (source) | Path to the directory where metadata and S3 objects are stored. |
| **`STIVA_S3_PORT`** | `9000` | Port the S3-compatible API server listens on. |
| **`STIVA_CONSOLE_PORT`**| `9001` | Port the Admin Web Console listens on. |
| **`STIVA_REGION`** | `us-east-1` | S3 region reported by the API. |
| **`STIVA_DOMAIN`** | *None* | Base domain for virtual-host style bucket requests (e.g., `mybucket.domain.com`). |
| **`STIVA_S3_ENDPOINT`** | *None* | Custom public S3 endpoint URL for generating presigned links in the console. |
| **`STIVA_TLS_ENABLED`** | `false` | Set to `true` to enable TLS/HTTPS for S3 API and Console. |
| **`STIVA_TLS_CERT`** | *None* | Absolute path to SSL/TLS certificate file. |
| **`STIVA_TLS_KEY`** | *None* | Absolute path to SSL/TLS private key file. |
| **`STIVA_JWT_SECRET`** | *Autogenerated* | Secret key for Console session signing. Recommended to set statically to preserve sessions. |
| **`STIVA_TRUST_PROXY`** | `false` | Trusts `X-Forwarded-For` proxy headers from reverse proxies (Nginx/Caddy/Cloudflare). |
| **`STIVA_LOGIN_RATE_LIMIT`**| `5` | Maximum console login attempts allowed per minute per IP address. |
| **`STIVA_API_RATE_LIMIT`**  | `60` | Maximum console API requests allowed per minute per IP address. |
| **`STIVA_METRICS_TOKEN`** | *None* | Bearer token required to scrape `/metrics`. If empty, requires a console JWT session. |
| **`STIVA_MAX_OBJECT_SIZE`** | `5368709120` (5GiB) | Maximum size in bytes accepted for a single object or multipart part, enforced by the storage engine regardless of signing mode. `0` disables the cap. |
| **`STIVA_LOG_LEVEL`** | `info` | Logging verbosity level (`debug`, `info`, `warn`, `error`). |
| **`STIVA_LOG_FORMAT`** | `text` | Log presentation format (`text` or `json`). |
| **`STIVA_SYNC_ENDPOINT`** | *None* | Remote S3 API endpoint URL target for asynchronous replication. |
| **`STIVA_SYNC_BUCKET`** | *None* | Remote target bucket name for asynchronous replication. |
| **`STIVA_SYNC_ACCESS_KEY`**| *None* | Remote credentials Access Key. |
| **`STIVA_SYNC_SECRET_KEY`**| *None* | Remote credentials Secret Key. |
| **`STIVA_SYNC_REGION`** | `us-east-1` | Remote target region. |
| **`STIVA_WEBHOOK_URL`** | *None* | Destination HTTP POST endpoint URL to receive webhook event payloads. |
| **`STIVA_WEBHOOK_SECRET`** | *None* | Shared secret used to sign outgoing webhook payloads (`X-Stiva-Signature: sha256=<hmac>`) so receivers can verify authenticity. |
| **`STIVA_DISABLE_MIN_PART_SIZE`**| `false` | Disables S3 5MB minimum multipart part size requirement (highly useful for dev/testing). |

### 🔄 Active-Passive Replication Configuration

Setting these variables enables automated, asynchronous background replication. Whenever objects are written (`PUT`) or deleted (`DELETE`) in Stiva, the operations are queue-dispatched to the replication target.

Objects are mirrored to **`<STIVA_SYNC_BUCKET>/<source-bucket>/<key>`**, so each source bucket occupies its own prefix in the target bucket and buckets sharing a key name cannot overwrite one another.

> **Note:** SSE-C encrypted objects are **not** replicated. Stiva deliberately never persists the customer-provided key, so it cannot read those objects back to send them; each is skipped with a warning in the logs.

### 🪝 Webhook Event Notifications Configuration

Set the `STIVA_WEBHOOK_URL` variable to dispatch event JSON payloads to a webhook listener.

#### Example Webhook Event Payload:
```json
{
  "eventName": "s3:ObjectCreated",
  "bucket": "assets",
  "key": "images/photo.png",
  "size": 24590,
  "etag": "a3b2c1...",
  "versionId": "b18ca72c-...",
  "time": "2026-06-11T15:43:00Z"
}
```

---

## 🛠️ Interacting with S3 Clients

You can use standard S3-compatible tools to interact with Stiva.

### AWS CLI

Configure the AWS CLI profile or override the endpoint directly:

```bash
# Set credentials environment
export AWS_ACCESS_KEY_ID=stiva
export AWS_SECRET_ACCESS_KEY=stiva123

# Create a bucket
aws --endpoint-url http://localhost:9000 s3 mb s3://test-bucket

# Upload a file
aws --endpoint-url http://localhost:9000 s3 cp myfile.txt s3://test-bucket/

# List files
aws --endpoint-url http://localhost:9000 s3 ls s3://test-bucket/
```

### rclone

Add a remote section to your `rclone.conf`:

```ini
[stiva]
type = s3
provider = Other
env_auth = false
access_key_id = stiva
secret_access_key = stiva123
endpoint = http://localhost:9000
```

And run:
```bash
rclone lsd stiva:
```

---

## 🧪 Running Tests

Unit, integration, and fuzz tests are located under the `tests/` directory (`tests/auth`, `tests/config`, `tests/console`, `tests/httpx`, `tests/s3api`, `tests/server`, `tests/storage`).

To run the test suite:

```bash
# Run all tests
go test -v ./...

# Run tests with race detection
go test -race ./...
```

---

## 🤝 Contributing

Contributions are welcome! Please see [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

## 📜 Changelog

Detailed release history and version changes can be found in [CHANGELOG.md](CHANGELOG.md).

## 🔐 Security

If you discover a security vulnerability, please see our [Security Policy](SECURITY.md).

## 📝 License

Distributed under the MIT License. See [LICENSE](LICENSE) for more information.

---

**Author**: [Salvatore Corvaglia](https://github.com/salvatorecorvaglia)