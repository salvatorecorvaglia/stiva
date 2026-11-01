package console

import (
	"fmt"
	"testing"
)

func TestAuditLimiterCardinality(t *testing.T) {
	r := newRateLimiter(5)
	for i := 0; i < 11000; i++ {
		r.allow(fmt.Sprint(i))
	}
	if len(r.clients) > 10000 {
		t.Fatalf("limiter entries=%d, exceeds intended 10000 bound", len(r.clients))
	}
}

func TestRateLimiterRetryAfterMatchesRefill(t *testing.T) {
	limiter := newRateLimiter(1)
	if !limiter.allow("client") || limiter.allow("client") {
		t.Fatal("expected second request to exhaust token bucket")
	}
	if delay := limiter.retryAfter("client"); delay < 59 || delay > 60 {
		t.Fatalf("Retry-After=%d; want next token in about a minute", delay)
	}
}
