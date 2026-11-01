package s3api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuditPreconditionWireResponse(t *testing.T) {
	rt, e, _ := newHardeningRouter(t)
	seedObject(t, e, "audit-bucket", "key", strings.Repeat("x", 4096), "application/octet-stream")
	e.SetBucketPublic("audit-bucket", true)
	srv := httptest.NewServer(rt)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/audit-bucket/key", nil)
	req.Header.Set("If-Match", `"wrong"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("HTTP %d Content-Length=%s actual bytes=%d read error=%v", resp.StatusCode, resp.Header.Get("Content-Length"), len(body), err)
	}
}

func TestRouterConfiguredPayloadLimit(t *testing.T) {
	for _, limit := range []int64{0, 1, 8 << 30} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			_, engine, creds := newHardeningRouter(t)
			router := NewRouter(RouterOptions{Engine: engine, Creds: creds, MaxPayloadSize: &limit})
			t.Cleanup(router.Close)
			req := httptest.NewRequest(http.MethodPut, "/audit/key", strings.NewReader("payload"))
			signPut(req, creds, "us-east-1", time.Now().UTC(), []byte("payload"))
			err := router.verifier.Verify(req)
			if limit == 1 && err == nil {
				t.Fatal("configured limit was ignored")
			}
			if limit != 1 && err != nil {
				t.Fatal(err)
			}
		})
	}
}
