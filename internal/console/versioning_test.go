package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestConsoleVersionHistory(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	if err := engine.CreateBucket("history"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.PutObject(context.Background(), "history", "legacy.txt", strings.NewReader("legacy"), 6, "text/plain"); err != nil {
		t.Fatal(err)
	}
	base := "/api/buckets/history"
	for _, path := range []string{"/versioning", "/versions"} {
		if w := do(t, h, http.MethodGet, base+path, "", nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
	}
	if w := do(t, h, http.MethodPost, base+"/versioning", "", []byte(`{"status":"Enabled"}`)); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated mutation: %d", w.Code)
	}
	if w := do(t, h, http.MethodPost, base+"/versioning", token, []byte(`{"status":"Enabled"}`)); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if w := do(t, h, http.MethodGet, base+"/versioning", token, nil); !strings.Contains(w.Body.String(), `"Enabled"`) {
		t.Fatal(w.Body.String())
	}
	old, err := engine.PutObject(context.Background(), "history", "report.txt", strings.NewReader("old"), 3, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.PutObject(context.Background(), "history", "report.txt", strings.NewReader("new"), 3, "text/plain"); err != nil {
		t.Fatal(err)
	}
	_, marker, err := engine.DeleteObject("history", "report.txt", "")
	if err != nil {
		t.Fatal(err)
	}

	// The history includes deleted objects and resumes within the same key.
	keyMarker, versionMarker := "", ""
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		q := url.Values{"prefix": {"report.txt"}, "maxKeys": {"1"}, "keyMarker": {keyMarker}, "versionIdMarker": {versionMarker}}
		w := do(t, h, http.MethodGet, base+"/versions?"+q.Encode(), token, nil)
		if w.Code != http.StatusOK {
			t.Fatal(w.Body.String())
		}
		if strings.Contains(w.Body.String(), "blobId") {
			t.Fatal("internal blob ID exposed")
		}
		var page struct {
			Items []struct {
				VersionID      string `json:"versionId"`
				IsDeleteMarker bool   `json:"isDeleteMarker"`
			} `json:"items"`
			Truncated bool   `json:"isTruncated"`
			Key       string `json:"nextKeyMarker"`
			Version   string `json:"nextVersionIdMarker"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if seen[item.VersionID] {
				t.Fatal("duplicate version across pages")
			}
			seen[item.VersionID] = true
			if item.VersionID == marker && !item.IsDeleteMarker {
				t.Fatal("missing delete marker flag")
			}
		}
		if !page.Truncated {
			break
		}
		keyMarker, versionMarker = page.Key, page.Version
	}
	if len(seen) != 3 || !seen[old.VersionID] || !seen[marker] {
		t.Fatalf("history: %v", seen)
	}

	// A deleted object's old version can still be streamed; a query parameter
	// cannot change the version bound into the signed download token.
	legacyBody, _ := json.Marshal(map[string]string{"key": "legacy.txt", "versionId": "null"})
	legacyLink := do(t, h, http.MethodPost, base+"/objects/download-link", token, legacyBody)
	if legacyLink.Code != http.StatusOK {
		t.Fatal(legacyLink.Body.String())
	}
	var legacyURL struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(legacyLink.Body.Bytes(), &legacyURL); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, http.MethodGet, legacyURL.URL, "", nil); w.Code != http.StatusOK || w.Body.String() != "legacy" {
		t.Fatalf("null version: %d %q", w.Code, w.Body.String())
	}
	body, _ := json.Marshal(map[string]string{"key": "report.txt", "versionId": old.VersionID})
	w := do(t, h, http.MethodPost, base+"/objects/download-link", token, body)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	var link struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &link); err != nil {
		t.Fatal(err)
	}
	w = do(t, h, http.MethodGet, link.URL+"?versionId="+url.QueryEscape(marker), "", nil)
	if w.Code != http.StatusOK || w.Body.String() != "old" {
		t.Fatalf("old version: %d %q", w.Code, w.Body.String())
	}
	if w = do(t, h, http.MethodGet, link.URL, "", nil); w.Code != http.StatusGone {
		t.Fatalf("reused link: %d", w.Code)
	}
	w = do(t, h, http.MethodGet, base+"/objects/download?key=report.txt&versionId="+url.QueryEscape(old.VersionID), token, nil)
	if w.Code != http.StatusOK || w.Body.String() != "old" {
		t.Fatalf("session version: %d %q", w.Code, w.Body.String())
	}
	body, _ = json.Marshal(map[string]string{"key": "report.txt", "versionId": "missing-version"})
	if w = do(t, h, http.MethodPost, base+"/objects/download-link", token, body); w.Code == http.StatusOK {
		t.Fatal("missing version accepted")
	}

	if w = do(t, h, http.MethodPost, base+"/versioning", token, []byte(`{"status":"Disabled"}`)); w.Code != http.StatusBadRequest {
		t.Fatalf("disable: %d", w.Code)
	}
	if w = do(t, h, http.MethodPost, base+"/versioning", token, []byte(`{"status":"Suspended"}`)); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if w = do(t, h, http.MethodGet, base+"/versioning", token, nil); !strings.Contains(w.Body.String(), `"Suspended"`) {
		t.Fatal(w.Body.String())
	}
	if w = do(t, h, http.MethodGet, "/api/buckets/nonexistent/versions", token, nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing bucket: %d", w.Code)
	}
}
