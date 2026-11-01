package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// These fixtures intentionally live outside the timed region. Run with
// go test ./internal/storage -run '^$' -bench BenchmarkStorage -benchmem.
func BenchmarkStorage(b *testing.B) {
	for _, operation := range []string{"listing", "versions", "usage", "publication", "multipart"} {
		b.Run(operation, func(b *testing.B) {
			engine, err := NewFilesystemEngine(b.TempDir(), nil, "")
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := engine.Close(); err != nil {
					b.Error(err)
				}
			})
			engine.SetMetrics(&MetricsTracker{})
			engine.SetDisableMinPartSize(true)
			if err := engine.CreateBucket("benchmark"); err != nil {
				b.Fatal(err)
			}
			if err := engine.SetBucketVersioning("benchmark", "Enabled"); err != nil {
				b.Fatal(err)
			}
			ctx := context.Background()
			for i := 0; i < 128; i++ {
				for version := 0; version < 2; version++ {
					if _, err := engine.PutObject(ctx, "benchmark", fmt.Sprint(i), strings.NewReader("fixture"), 7, "text/plain"); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				switch operation {
				case "listing":
					_, err = engine.ListObjects(&ListObjectsInput{Bucket: "benchmark", MaxKeys: 50})
				case "versions":
					_, err = engine.ListObjectVersions(&ListVersionsInput{Bucket: "benchmark", MaxKeys: 50})
				case "usage":
					_, err = engine.CollectStorageUsage(ctx)
				case "publication":
					_, err = engine.PutObject(ctx, "benchmark", "publication", strings.NewReader("payload"), 7, "text/plain")
				case "multipart":
					var upload *MultipartUploadInfo
					upload, err = engine.CreateMultipartUpload(ctx, "benchmark", "assembled", "application/octet-stream")
					if err == nil {
						var part *PartInfo
						part, err = engine.UploadPart(ctx, "benchmark", "assembled", upload.UploadID, 1, strings.NewReader("payload"), 7)
						if err == nil {
							_, err = engine.CompleteMultipartUpload(ctx, "benchmark", "assembled", upload.UploadID, []CompletePart{{PartNumber: 1, ETag: part.ETag}})
						}
					}
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
