package storage

import (
	"bytes"
	"compress/gzip"
	"testing"
)

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestPooledGzipReaderDoubleCloseReturnsOnce is the regression test for a
// pooled gzip.Reader being handed to two callers at once. Closing a
// pooledGzipReader twice — which replication did on every compressed object,
// once via net/http closing the request body and once via its own defer —
// put the same reader into the pool twice, so two later GetObject calls could
// decompress through one shared reader: a data race that can serve corrupted
// object content.
func TestPooledGzipReaderDoubleCloseReturnsOnce(t *testing.T) {
	data := gzipped(t, "payload")

	for i := 0; i < 50; i++ {
		gr, err := getGzipReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		p := &pooledGzipReader{Reader: gr}
		_ = p.Close()
		_ = p.Close()

		a, err := getGzipReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		b, err := getGzipReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if a == b {
			t.Fatalf("iteration %d: the pool handed out the same gzip.Reader twice after a double Close", i)
		}
		putGzipReader(a)
		putGzipReader(b)
	}
}
