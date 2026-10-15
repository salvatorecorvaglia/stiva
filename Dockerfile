# ================================================================
# Stiva — Multi-stage Docker Build
# ================================================================
# Stage 1: Build the Go binary
# Stage 2: Package into a minimal runtime image
# ================================================================

# --- Builder Stage ---
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

RUN apk add --no-cache git ca-certificates

WORKDIR /build

# Copy module files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build a statically-linked binary with native cross-compilation
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-w -s" \
    -o stiva \
    ./cmd/stiva

# --- Runtime Stage ---
FROM alpine:3.22

# wget is used by the container healthcheck.
RUN apk add --no-cache ca-certificates tzdata wget

# Create non-root user with a fixed UID/GID: adduser -S assigns whatever the
# next free system UID happens to be, which isn't guaranteed stable across
# image rebuilds or Alpine versions and can cause volume permission drift
# between image versions for orchestrators that pin runAsUser.
RUN addgroup -S -g 1000 stiva && adduser -S -u 1000 -G stiva stiva

# Create data directory
RUN mkdir -p /data && chown stiva:stiva /data

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/stiva .

# Set default environment variables
ENV STIVA_DATA_DIR=/data \
    STIVA_S3_PORT=9000 \
    STIVA_CONSOLE_PORT=9001 \
    STIVA_REGION=us-east-1

# Expose ports
EXPOSE 9000 9001

# Use data directory as volume
VOLUME ["/data"]

# Run as non-root user
USER stiva

# Probes the unauthenticated /_health endpoint on both servers. The old check
# only fetched the console's index page over plain HTTP, so it never noticed a
# dead S3 API and always failed once TLS was enabled. Each port is tried over
# HTTP, then HTTPS; certificate verification is skipped because the
# self-signed mode (STIVA_TLS_ENABLED without a cert) is a supported setup.
HEALTHCHECK --interval=30s --timeout=10s --start-period=10s --retries=3 \
    CMD sh -c 'for port in "$STIVA_S3_PORT" "$STIVA_CONSOLE_PORT"; do \
        wget -q -T 3 --tries=1 --spider "http://127.0.0.1:$port/_health" || \
        wget -q -T 3 --tries=1 --spider --no-check-certificate "https://127.0.0.1:$port/_health" || exit 1; \
    done'

ENTRYPOINT ["./stiva"]
