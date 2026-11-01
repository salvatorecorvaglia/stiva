package s3api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// End-to-end signed PUT/GET pairs include HTTP parsing, authentication, durable
// publication, response transfer, and payload verification. Latency samples
// are bounded even when Go calibrates a large benchmark iteration count.
func BenchmarkConcurrentS3RoundTrip(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20} {
		for _, workers := range []int{4, 16} {
			b.Run(fmt.Sprintf("bytes-%d/workers-%d", size, workers), func(b *testing.B) {
				engine, err := storage.NewFilesystemEngine(b.TempDir(), nil, "")
				if err != nil {
					b.Fatal(err)
				}
				engine.SetMetrics(&storage.MetricsTracker{})
				b.Cleanup(func() {
					if err := engine.Close(); err != nil {
						b.Error(err)
					}
				})
				if err := engine.CreateBucket("benchmark"); err != nil {
					b.Fatal(err)
				}
				creds := auth.NewCredentials("benchmark", "benchmark-secret")
				router := NewRouter(RouterOptions{Engine: engine, Creds: creds, Region: "us-east-1", Metrics: engine.Metrics()})
				b.Cleanup(router.Close)
				server := httptest.NewServer(router)
				b.Cleanup(server.Close)
				transport := &http.Transport{MaxIdleConnsPerHost: workers, MaxConnsPerHost: workers}
				b.Cleanup(transport.CloseIdleConnections)
				client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
				samples := make([]time.Duration, min(b.N, 10000))
				payloads := make([][]byte, workers)
				downloads := make([][]byte, workers)
				for i := range payloads {
					payloads[i] = bytes.Repeat([]byte{byte(i)}, size)
					downloads[i] = make([]byte, size)
				}
				var sequence atomic.Int64
				var group sync.WaitGroup
				b.SetBytes(int64(2 * size))
				b.ReportAllocs()
				b.ResetTimer()
				for worker := 0; worker < workers; worker++ {
					group.Add(1)
					go func(worker int) {
						defer group.Done()
						endpoint := fmt.Sprintf("%s/benchmark/worker-%d", server.URL, worker)
						for {
							index := int(sequence.Add(1) - 1)
							if index >= b.N {
								return
							}
							start := time.Now()
							for _, method := range []string{http.MethodPut, http.MethodGet} {
								var body io.Reader
								if method == http.MethodPut {
									body = bytes.NewReader(payloads[worker])
								}
								req, err := http.NewRequestWithContext(context.Background(), method, endpoint, body)
								if err != nil {
									b.Error(err)
									return
								}
								req.Header.Set("Content-Type", "application/octet-stream")
								auth.SignRequest(req, creds.AccessKey, creds.SecretKey, "us-east-1", "s3")
								response, err := client.Do(req)
								if err != nil {
									b.Error(err)
									return
								}
								var readErr error
								if method == http.MethodGet {
									_, readErr = io.ReadFull(response.Body, downloads[worker])
									if readErr == nil && response.ContentLength != int64(size) {
										readErr = fmt.Errorf("unexpected response length %d", response.ContentLength)
									}
								} else {
									_, readErr = io.Copy(io.Discard, response.Body)
								}
								closeErr := response.Body.Close()
								if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK {
									b.Errorf("%s: HTTP %d, read=%v close=%v", method, response.StatusCode, readErr, closeErr)
									return
								}
								if method == http.MethodGet && !bytes.Equal(downloads[worker], payloads[worker]) {
									b.Error("download differs from acknowledged upload")
									return
								}
							}
							if index < len(samples) {
								samples[index] = time.Since(start)
							}
						}
					}(worker)
				}
				group.Wait()
				b.StopTimer()
				sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
				if len(samples) > 0 {
					b.ReportMetric(float64(samples[(len(samples)-1)*95/100])/float64(time.Millisecond), "pair-p95-ms")
				}
			})
		}
	}
}
