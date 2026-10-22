package httpx

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

type readinessStub struct{ err error }

func (r readinessStub) CheckReady() error { return r.err }
func TestReadiness(t *testing.T) {
	for _, tc := range []struct {
		engine any
		status int
	}{{readinessStub{}, 200}, {readinessStub{errors.New("secret disk path")}, 503}, {struct{}{}, 503}} {
		w := httptest.NewRecorder()
		ReadinessHandler(tc.engine)(w, httptest.NewRequest("GET", "/_ready", nil))
		if w.Code != tc.status {
			t.Fatalf("status=%d want=%d", w.Code, tc.status)
		}
		if strings.Contains(w.Body.String(), "secret") {
			t.Fatal("readiness leaked storage error")
		}
	}
}
