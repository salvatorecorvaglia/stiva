package storage

import (
	"bytes"
	"context"
	"io"
	"testing"
)

// TestSSECReaderSeekCurrentAfterUnalignedSeek is the regression test for the
// decrypting reader's position bookkeeping. Seeking to an offset that is not a
// multiple of the AES block size read the remainder bytes to reach it, and
// counted them on top of an offset already set to the target — so
// Seek(0, io.SeekCurrent) reported a position ahead of the real one.
func TestSSECReaderSeekCurrentAfterUnalignedSeek(t *testing.T) {
	engine := setupTestEngine(t)
	if err := engine.CreateBucket("ctr"); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), SSECContextKey, ssecParamsForKey(bytes.Repeat([]byte{0x55}, 32)))

	data := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	if _, err := engine.PutObject(ctx, "ctr", "obj", bytes.NewReader(data), int64(len(data)), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}

	r, _, err := engine.GetObject(ctx, "ctr", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rs, ok := r.(io.ReadSeeker)
	if !ok {
		t.Fatal("SSE-C object reader is not seekable")
	}

	if _, err := rs.Seek(21, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	pos, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 21 {
		t.Fatalf("Seek(0, SeekCurrent) = %d after seeking to 21", pos)
	}

	if _, err := rs.Seek(-3, io.SeekCurrent); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(rs, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data[18:22]) {
		t.Fatalf("read %q after relative seek, want %q", got, data[18:22])
	}
}
