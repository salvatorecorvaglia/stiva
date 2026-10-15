package console

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/auth"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

func uploadForm(t *testing.T, h *Handler, token, bucket, key, content string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "upload.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte(content))
	_ = mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/buckets/"+bucket+"/objects/upload?key="+key, &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func errorMessage(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Error
}

// TestUploadReportsStorageErrors is the regression test for console uploads
// failing with a bare 500 "upload failed" whatever went wrong, so the user
// was never told why (a missing bucket, an over-long key, an oversized
// file). Storage errors now reach the UI with a status and message.
func TestUploadReportsStorageErrors(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	if err := engine.CreateBucket("uploads"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, bucket, key string
		status            int
		message           string
	}{
		{"missing bucket", "nope", "a.txt", http.StatusNotFound, "bucket does not exist"},
		{"key too long", "uploads", strings.Repeat("k", 1025), http.StatusBadRequest, "key is too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := uploadForm(t, h, token, tc.bucket, tc.key, "data")
			if w.Code != tc.status || !strings.Contains(errorMessage(t, w), tc.message) {
				t.Fatalf("status %d error %q; want %d containing %q", w.Code, errorMessage(t, w), tc.status, tc.message)
			}
		})
	}
}

// TestDownloadReportsErrorsAccurately: every download failure used to be
// reported as 404 "object not found" — including an SSE-C object, which does
// exist but cannot be read without its key.
func TestDownloadReportsErrorsAccurately(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	if err := engine.CreateBucket("downloads"); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	sse := &storage.SSECParams{Algorithm: "AES256", Key: key, KeyMD5: "k"}
	ctx := context.WithValue(context.Background(), storage.SSECContextKey, sse)
	if _, err := engine.PutObject(ctx, "downloads", "secret.bin", strings.NewReader("x"), 1, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}

	if w := do(t, h, http.MethodGet, "/api/buckets/downloads/objects/download?key=absent", token, nil); w.Code != http.StatusNotFound {
		t.Errorf("missing object = %d, want 404", w.Code)
	}
	w := do(t, h, http.MethodGet, "/api/buckets/downloads/objects/download?key=secret.bin", token, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(errorMessage(t, w), "Customer-provided Keys") {
		t.Errorf("SSE-C object = %d %q; want 400 explaining the object is encrypted with a customer key", w.Code, errorMessage(t, w))
	}
}

func issueDownloadLink(t *testing.T, h *Handler, token, bucket, key string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"key": key})
	w := do(t, h, http.MethodPost, "/api/buckets/"+bucket+"/objects/download-link", token, body)
	if w.Code != http.StatusOK {
		t.Fatalf("download-link = %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.URL
}

// TestDownloadLinks covers the link the console now downloads through. The
// UI used to fetch the whole object into browser memory (resp.blob()) before
// saving it, which exhausted memory on large files. A short-lived,
// single-use link lets the browser stream the download itself.
func TestDownloadLinks(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	if err := engine.CreateBucket("links"); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("0123456789", 100)
	if _, err := engine.PutObject(context.Background(), "links", "dir/report.bin", strings.NewReader(content), int64(len(content)), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}

	t.Run("streams without a session, once", func(t *testing.T) {
		url := issueDownloadLink(t, h, token, "links", "dir/report.bin")
		if !strings.HasPrefix(url, "/api/download/") {
			t.Fatalf("link %q is not a console download URL", url)
		}

		r := httptest.NewRequest(http.MethodGet, url, nil)
		r.Header.Set("Range", "bytes=10-19")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r) // no Authorization: a browser navigation carries none
		if w.Code != http.StatusPartialContent || w.Body.String() != content[10:20] {
			t.Fatalf("download = %d %q, want 206 %q", w.Code, w.Body.String(), content[10:20])
		}
		if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "report.bin") {
			t.Errorf("Content-Disposition = %q, want an attachment named report.bin", cd)
		}

		if w := do(t, h, http.MethodGet, url, "", nil); w.Code != http.StatusGone {
			t.Errorf("second use = %d, want 410: links are single-use", w.Code)
		}
	})

	t.Run("requires a session to issue", func(t *testing.T) {
		body, _ := json.Marshal(map[string]string{"key": "dir/report.bin"})
		if w := do(t, h, http.MethodPost, "/api/buckets/links/objects/download-link", "", body); w.Code != http.StatusUnauthorized {
			t.Errorf("issuing without a session = %d, want 401", w.Code)
		}
	})

	t.Run("rejects expired, forged and foreign links", func(t *testing.T) {
		expired, err := h.tokens.generateDownload(h.creds, "links", "dir/report.bin", time.Now().Add(-2*downloadLinkTTL))
		if err != nil {
			t.Fatal(err)
		}
		other := NewHandler(Options{Engine: engine, Creds: auth.NewCredentials("access", "rotated"), JWTSecret: h.tokens.secret, LoginRateLimit: 5, APIRateLimit: 100})
		foreign := strings.TrimPrefix(issueDownloadLink(t, h, token, "links", "dir/report.bin"), "/api/download/")

		for name, tc := range map[string]struct {
			h     *Handler
			token string
		}{
			"expired":            {h, expired},
			"forged":             {h, expired[:len(expired)-4] + "AAAA"},
			"after key rotation": {other, foreign},
		} {
			if w := do(t, tc.h, http.MethodGet, "/api/download/"+tc.token, "", nil); w.Code != http.StatusUnauthorized {
				t.Errorf("%s link = %d, want 401", name, w.Code)
			}
		}
	})

	t.Run("is not a session", func(t *testing.T) {
		link := strings.TrimPrefix(issueDownloadLink(t, h, token, "links", "dir/report.bin"), "/api/download/")
		if w := do(t, h, http.MethodGet, "/api/buckets", link, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("a download link used as a session token = %d, want 401", w.Code)
		}
	})
}

// TestBatchDelete covers the endpoint behind multi-select and folder delete,
// which used to send one DELETE request per object.
func TestBatchDelete(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	if err := engine.CreateBucket("batch"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, err := engine.PutObject(context.Background(), "batch", k, strings.NewReader("x"), 1, "text/plain"); err != nil {
			t.Fatal(err)
		}
	}

	body, _ := json.Marshal(map[string][]string{"keys": {"a", "b", ""}})
	w := do(t, h, http.MethodPost, "/api/buckets/batch/objects/delete", token, body)
	if w.Code != http.StatusOK {
		t.Fatalf("batch delete = %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Deleted []string `json:"deleted"`
		Errors  []struct {
			Key   string `json:"key"`
			Error string `json:"error"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if strings.Join(resp.Deleted, ",") != "a,b" || len(resp.Errors) != 1 || resp.Errors[0].Key != "" {
		t.Fatalf("response %s; want a and b deleted and one error for the empty key", w.Body.String())
	}
	if n, _ := engine.CountObjects("batch"); n != 1 {
		t.Errorf("%d objects left, want 1", n)
	}

	tooMany := make([]string, maxBatchDelete+1)
	for i := range tooMany {
		tooMany[i] = "k"
	}
	body, _ = json.Marshal(map[string][]string{"keys": tooMany})
	if w := do(t, h, http.MethodPost, "/api/buckets/batch/objects/delete", token, body); w.Code != http.StatusBadRequest {
		t.Errorf("oversized batch = %d, want 400", w.Code)
	}
}

// TestRecursiveListing: folder delete needs every key under a prefix, not
// the rolled-up folder view the browser shows.
func TestRecursiveListing(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	if err := engine.CreateBucket("tree"); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"dir/a", "dir/sub/b", "dir/sub/deeper/c", "other"} {
		if _, err := engine.PutObject(context.Background(), "tree", k, strings.NewReader("x"), 1, "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	w := do(t, h, http.MethodGet, "/api/buckets/tree/objects?prefix=dir/&recursive=1", token, nil)
	var resp struct {
		Items []struct {
			Key      string `json:"key"`
			IsPrefix bool   `json:"isPrefix"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, it := range resp.Items {
		if it.IsPrefix {
			t.Errorf("recursive listing returned a folder %q", it.Key)
		}
		keys = append(keys, it.Key)
	}
	if strings.Join(keys, ",") != "dir/a,dir/sub/b,dir/sub/deeper/c" {
		t.Errorf("recursive listing = %v", keys)
	}
}
