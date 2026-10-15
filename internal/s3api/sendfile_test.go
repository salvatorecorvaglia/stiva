package s3api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// readFromRecorder is a ResponseWriter whose ReadFrom records the source it
// was handed, standing in for net/http's, which passes it to the kernel
// (sendfile) only when it is a file or an io.LimitedReader over one.
type readFromRecorder struct {
	*httptest.ResponseRecorder
	sources []io.Reader
}

func (r *readFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.sources = append(r.sources, src)
	return io.Copy(r.ResponseRecorder, src)
}

// TestPlainObjectDownloadReachesSendfile is the regression test for object
// downloads always being copied through userspace. The router's response
// writer forwarded ReadFrom precisely to keep net/http's sendfile path, but
// the engine wrapped the object's *os.File in two layers of readers, so what
// reached ReadFrom was never a file and the fast path never ran.
func TestPlainObjectDownloadReachesSendfile(t *testing.T) {
	rt, eng := newOpsRouter(t)
	putObjects(t, eng, "fast")
	body := strings.Repeat("s", 64<<10)
	if _, err := eng.PutObject(context.Background(), "fast", "blob.bin", strings.NewReader(body), int64(len(body)), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetBucketPublic("fast", true); err != nil {
		t.Fatal(err)
	}

	before := atomic.LoadUint64(&storage.GlobalMetrics.BytesDownloaded)
	w := &readFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/fast/blob.bin", nil)
	req.Header.Set("Range", "bytes=100-4195")
	rt.ServeHTTP(w, req)

	if w.Code != http.StatusPartialContent || w.Body.String() != body[100:4196] {
		t.Fatalf("status %d, %d bytes; want 206 with bytes 100-4195", w.Code, w.Body.Len())
	}
	if len(w.sources) != 1 {
		t.Fatalf("ReadFrom called %d times, want 1", len(w.sources))
	}
	lr, ok := w.sources[0].(*io.LimitedReader)
	if !ok {
		t.Fatalf("ReadFrom got %T, want *io.LimitedReader", w.sources[0])
	}
	if _, ok := lr.R.(*os.File); !ok {
		t.Fatalf("ReadFrom got a LimitedReader over %T; net/http only uses sendfile for *os.File", lr.R)
	}

	// The download is still counted, now at the writer.
	if got := atomic.LoadUint64(&storage.GlobalMetrics.BytesDownloaded) - before; got != 4096 {
		t.Errorf("BytesDownloaded rose by %d, want 4096", got)
	}
}
