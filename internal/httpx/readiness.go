package httpx

import "net/http"

// ReadinessHandler deliberately hides storage errors from unauthenticated callers.
func ReadinessHandler(engine any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		checker, ok := engine.(interface{ CheckReady() error })
		if !ok || checker.CheckReady() != nil {
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}
