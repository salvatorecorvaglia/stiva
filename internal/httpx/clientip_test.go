package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		name        string
		remoteAddr  string
		xff         string
		trustProxy  bool
		trustedHops int
		want        string
	}{
		{
			name:       "proxy not trusted uses peer address",
			remoteAddr: "10.0.0.5:1234",
			xff:        "1.2.3.4",
			trustProxy: false,
			want:       "10.0.0.5",
		},
		{
			name:       "no forwarded header falls back to peer",
			remoteAddr: "10.0.0.5:1234",
			trustProxy: true,
			want:       "10.0.0.5",
		},
		{
			// The whole point of the fix: a client that injects its own
			// X-Forwarded-For must not be able to choose the rate-limit key.
			name:        "spoofed leading entries are ignored",
			remoteAddr:  "10.0.0.5:1234",
			xff:         "9.9.9.9, 8.8.8.8, 203.0.113.7",
			trustProxy:  true,
			trustedHops: 1,
			want:        "203.0.113.7",
		},
		{
			name:        "single proxy reports the real client",
			remoteAddr:  "10.0.0.5:1234",
			xff:         "203.0.113.7",
			trustProxy:  true,
			trustedHops: 1,
			want:        "203.0.113.7",
		},
		{
			name:        "two trusted hops walks one further left",
			remoteAddr:  "10.0.0.5:1234",
			xff:         "203.0.113.7, 10.1.1.1",
			trustProxy:  true,
			trustedHops: 2,
			want:        "203.0.113.7",
		},
		{
			name:        "chain shorter than hop count falls back to peer",
			remoteAddr:  "10.0.0.5:1234",
			xff:         "203.0.113.7",
			trustProxy:  true,
			trustedHops: 3,
			want:        "10.0.0.5",
		},
		{
			name:        "non-IP entry falls back to peer",
			remoteAddr:  "10.0.0.5:1234",
			xff:         "not-an-ip",
			trustProxy:  true,
			trustedHops: 1,
			want:        "10.0.0.5",
		},
		{
			name:        "zero hops is treated as one",
			remoteAddr:  "10.0.0.5:1234",
			xff:         "9.9.9.9, 203.0.113.7",
			trustProxy:  true,
			trustedHops: 0,
			want:        "203.0.113.7",
		},
		{
			name:       "remote addr without port",
			remoteAddr: "10.0.0.5",
			trustProxy: false,
			want:       "10.0.0.5",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := ClientIP(r, tc.trustProxy, tc.trustedHops); got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientIPRateLimitBypass demonstrates the concrete attack the fix closes:
// rotating the leading X-Forwarded-For entry must not yield a fresh rate-limit
// bucket on every request.
func TestClientIPRateLimitBypass(t *testing.T) {
	seen := map[string]bool{}
	for _, spoof := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"} {
		r := httptest.NewRequest("POST", "/api/login", nil)
		r.RemoteAddr = "10.0.0.5:1234"
		r.Header.Set("X-Forwarded-For", spoof+", 203.0.113.7")
		seen[ClientIP(r, true, 1)] = true
	}
	if len(seen) != 1 {
		t.Errorf("attacker obtained %d distinct rate-limit keys, want 1: %v", len(seen), seen)
	}
}
