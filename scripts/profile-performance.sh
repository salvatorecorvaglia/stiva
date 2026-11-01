#!/usr/bin/env bash
# Reproducible local benchmarks; all data, test binaries and profiles stay in tmp.
set -euo pipefail
output=${STIVA_PROFILE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/stiva-profile.XXXXXX")}
mkdir -p "$output"
go version > "$output/environment.txt"
go env GOOS GOARCH >> "$output/environment.txt"

go test ./internal/storage -run '^$' -bench '^BenchmarkConcurrentPublication$' -benchtime=32x -count=1 -benchmem > "$output/concurrent-publication.txt" 2>&1
go test ./internal/storage -run '^$' -bench '^BenchmarkLargeBucketMetadata$' -benchtime=3x -count=1 -benchmem > "$output/large-metadata.txt" 2>&1
go test ./internal/s3api -run '^$' -bench '^BenchmarkConcurrentS3RoundTrip$' -benchtime=32x -count=1 -benchmem > "$output/s3-round-trip.txt" 2>&1

go test ./internal/storage -run '^$' -bench '^BenchmarkLargeBucketMetadata$/keys-10000$/usage$' -benchtime=2s -count=1 -cpuprofile="$output/metadata-cpu.pprof" -memprofile="$output/metadata-memory.pprof" -o "$output/metadata.test" > "$output/metadata-profile.txt" 2>&1
go test ./internal/s3api -run '^$' -bench '^BenchmarkConcurrentS3RoundTrip$/bytes-1048576$/workers-16$' -benchtime=2s -count=1 -cpuprofile="$output/s3-cpu.pprof" -memprofile="$output/s3-memory.pprof" -mutexprofile="$output/s3-mutex.pprof" -blockprofile="$output/s3-block.pprof" -o "$output/s3.test" > "$output/s3-profile.txt" 2>&1

for workload in metadata s3; do
    go tool pprof -top -nodecount=12 "$output/$workload-cpu.pprof" > "$output/$workload-cpu-top.txt"
    go tool pprof -top -alloc_space -nodecount=12 "$output/$workload-memory.pprof" > "$output/$workload-allocations-top.txt"
done
go tool pprof -top -nodecount=12 "$output/s3-mutex.pprof" > "$output/s3-locks-top.txt"
go tool pprof -top -nodecount=12 "$output/s3-block.pprof" > "$output/s3-blocking-top.txt"
printf 'Benchmark results and profiles: %s\n' "$output"
