package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

func TestUsageEndpointServesAuthenticatedCachedSnapshots(t *testing.T) {
	h, engine, token := newHandlerFixture(t)
	h.usage = storage.NewUsageTracker(engine)
	path := "/api/storage/usage"
	if w := do(t, h, http.MethodGet, path, "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, path, token, nil); w.Code != http.StatusAccepted {
		t.Fatalf("initial: %d %s", w.Code, w.Body.String())
	}
	if err := engine.CreateBucket("cached"); err != nil {
		t.Fatal(err)
	}
	if err := h.usage.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Changing the engine does not trigger a scan inside an HTTP request.
	if _, err := engine.PutObject(context.Background(), "cached", "new", strings.NewReader("data"), 4, "text/plain"); err != nil {
		t.Fatal(err)
	}
	read := func() storage.UsageSnapshot {
		t.Helper()
		w := do(t, h, http.MethodGet, path, token, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("usage: %d %s", w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("summary can be cached by browser")
		}
		var snapshot storage.UsageSnapshot
		if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	if got := read(); got.Usage.Objects != 0 || got.UpdatedAt == nil {
		t.Fatalf("cached: %+v", got)
	}
	if err := h.usage.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.Usage.Objects != 1 || got.Usage.LogicalBytes != 4 || len(got.Usage.Buckets) != 1 {
		t.Fatalf("updated: %+v", got.Usage)
	}
}
