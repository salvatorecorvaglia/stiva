package console_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/console"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

func newLimitsHandler(t *testing.T) (*console.Handler, storage.Engine, string) {
	t.Helper()
	engine, err := storage.NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	h := console.NewHandler(console.Options{
		Engine:         engine,
		Creds:          auth.NewCredentials("access", "secret"),
		S3Port:         9000,
		Region:         "us-east-1",
		LoginRateLimit: 1000,
		APIRateLimit:   10000,
	})
	token, err := console.GenerateToken("access")
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return h, engine, token
}

// TestLoginRejectsOversizedBody covers an unbounded json.Decoder on the one
// console endpoint that requires no authentication. Its body had no ceiling at
// all, unlike every equivalent on the S3 side.
func TestLoginRejectsOversizedBody(t *testing.T) {
	h, _, _ := newLimitsHandler(t)

	// Valid JSON, but with a secretKey far beyond any real credential.
	var body bytes.Buffer
	body.WriteString(`{"accessKey":"access","secretKey":"`)
	body.WriteString(strings.Repeat("A", 4<<20)) // 4MB
	body.WriteString(`"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body truncated at the limit, so decoding fails)", w.Code)
	}
	if w.Code == http.StatusOK {
		t.Error("oversized login body was accepted")
	}
}

// TestLoginStillAcceptsNormalBody guards the limit above against being so tight
// that a real login breaks.
func TestLoginStillAcceptsNormalBody(t *testing.T) {
	h, _, _ := newLimitsHandler(t)

	body, _ := json.Marshal(map[string]string{"accessKey": "access", "secretKey": "secret"})
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}

// TestUploadPartRejectsOutOfRangePartNumber covers the missing S3 part-number
// ceiling. Parts live in the upload's metadata record, which is rewritten whole
// on every part, so an unbounded part-number space meant unbounded metadata
// growth and quadratic metadata writes.
func TestUploadPartRejectsOutOfRangePartNumber(t *testing.T) {
	h, engine, token := newLimitsHandler(t)
	if err := engine.CreateBucket("parts"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	up, err := engine.CreateMultipartUpload("parts", "big.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}

	for _, partNumber := range []string{"0", "-1", "10001", "999999999"} {
		t.Run("part "+partNumber, func(t *testing.T) {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			_ = mw.WriteField("uploadId", up.UploadID)
			_ = mw.WriteField("key", "big.bin")
			_ = mw.WriteField("partNumber", partNumber)
			fw, _ := mw.CreateFormFile("file", "chunk")
			_, _ = fw.Write([]byte("data"))
			mw.Close()

			req := httptest.NewRequest(http.MethodPost, "/api/buckets/parts/multipart/upload-part", &buf)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for part number %s (body: %s)", w.Code, partNumber, w.Body.String())
			}
		})
	}
}

// TestEngineRejectsOutOfRangePartNumber pins the bound at the engine, so it
// holds for the S3 API and the console alike rather than only where a handler
// remembers to check.
func TestEngineRejectsOutOfRangePartNumber(t *testing.T) {
	_, engine, _ := newLimitsHandler(t)
	if err := engine.CreateBucket("parts"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	up, err := engine.CreateMultipartUpload("parts", "big.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}

	for _, partNumber := range []int{0, -1, storage.MaxPartNumber + 1, 1 << 30} {
		_, err := engine.UploadPart(context.Background(), "parts", "big.bin", up.UploadID,
			partNumber, strings.NewReader("data"), 4)
		if err == nil {
			t.Errorf("part number %d was accepted, want rejection", partNumber)
		}
	}

	// The valid boundaries must still work.
	for _, partNumber := range []int{1, storage.MaxPartNumber} {
		if _, err := engine.UploadPart(context.Background(), "parts", "big.bin", up.UploadID,
			partNumber, strings.NewReader("data"), 4); err != nil {
			t.Errorf("part number %d should be accepted, got: %v", partNumber, err)
		}
	}
}

// TestUploadPartStreamsLargeChunk exercises the streamed part upload with a
// chunk well past ParseMultipartForm's old 10MB in-memory threshold, which used
// to push the remainder into the *OS* temp directory, ignoring STIVA_DATA_DIR
// — the fix applied to uploadObject but never to its sibling, even though parts
// are the larger of the two by design.
//
// The leftover-files check below is a regression guard, not a proof: the old
// path deleted its spooled files via MultipartForm.RemoveAll before returning,
// so it passed too. What this pins is that a large part still round-trips
// correctly through the streaming path.
func TestUploadPartStreamsLargeChunk(t *testing.T) {
	osTemp := t.TempDir()
	// os.TempDir() consults TMPDIR on unix and TMP/TEMP on Windows.
	t.Setenv("TMPDIR", osTemp)
	t.Setenv("TMP", osTemp)
	t.Setenv("TEMP", osTemp)

	h, engine, token := newLimitsHandler(t)
	if err := engine.CreateBucket("parts"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	up, err := engine.CreateMultipartUpload("parts", "big.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}

	// Comfortably over ParseMultipartForm's 10MB in-memory threshold.
	chunk := bytes.Repeat([]byte("z"), 12<<20)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("uploadId", up.UploadID)
	_ = mw.WriteField("key", "big.bin")
	_ = mw.WriteField("partNumber", "1")
	fw, _ := mw.CreateFormFile("file", "chunk")
	_, _ = fw.Write(chunk)
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/buckets/parts/multipart/upload-part", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	entries, err := os.ReadDir(osTemp)
	if err != nil {
		t.Fatalf("read OS temp dir: %v", err)
	}
	var spooled []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "multipart-") {
			spooled = append(spooled, e.Name())
		}
	}
	if len(spooled) > 0 {
		t.Errorf("part was spooled into the OS temp dir instead of streaming: %v", spooled)
	}

	// And the part actually landed.
	parts, err := engine.ListParts("parts", "big.bin", up.UploadID)
	if err != nil {
		t.Fatalf("list parts: %v", err)
	}
	if len(parts) != 1 || parts[0].Size != int64(len(chunk)) {
		t.Errorf("part not stored correctly: %+v", parts)
	}
}

// TestUploadPartRequiresFieldsBeforeFile documents the ordering that streaming
// imposes, so a client sending the file first gets a clear error rather than a
// misfiled chunk.
func TestUploadPartRequiresFieldsBeforeFile(t *testing.T) {
	h, engine, token := newLimitsHandler(t)
	if err := engine.CreateBucket("parts"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	up, err := engine.CreateMultipartUpload("parts", "big.bin", "application/octet-stream")
	if err != nil {
		t.Fatalf("initiate: %v", err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "chunk") // file first
	_, _ = fw.Write([]byte("data"))
	_ = mw.WriteField("uploadId", up.UploadID)
	_ = mw.WriteField("key", "big.bin")
	_ = mw.WriteField("partNumber", "1")
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/buckets/parts/multipart/upload-part", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "before the file part") {
		t.Errorf("error should explain the field ordering, got: %s", w.Body.String())
	}
}

// TestCompleteMultipartRejectsTooManyParts covers the unbounded parts array the
// console accepted when completing an upload.
func TestCompleteMultipartRejectsTooManyParts(t *testing.T) {
	h, engine, token := newLimitsHandler(t)
	if err := engine.CreateBucket("parts"); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	parts := make([]map[string]interface{}, 0, storage.MaxPartNumber+1)
	for i := 1; i <= storage.MaxPartNumber+1; i++ {
		parts = append(parts, map[string]interface{}{"partNumber": i, "etag": fmt.Sprintf("%032x", i)})
	}
	body, _ := json.Marshal(map[string]interface{}{
		"uploadId": "does-not-matter", "key": "big.bin", "parts": parts,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/buckets/parts/multipart/complete", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an oversized parts array (body: %s)", w.Code, w.Body.String())
	}
}
