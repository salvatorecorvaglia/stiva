package httpx_test

import (
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/httpx"
)

// TestMaxKeysClamps pins the shared page-size bound. It lives in httpx because
// the console applied no ceiling at all while the S3 API clamped at 1000, so a
// console caller could ask the engine to accumulate an unbounded number of
// objects in memory. Both now go through this one function.
func TestMaxKeysClamps(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		def  int
		want int
	}{
		{"absent falls back to default", "", 1000, 1000},
		{"absent honours a smaller default", "", 50, 50},
		{"ordinary value passes through", "250", 1000, 250},
		{"exactly at the ceiling", "1000", 1000, 1000},
		{"above the ceiling is clamped", "5000", 1000, 1000},
		{"far above the ceiling is clamped", "999999999", 1000, 1000},
		{"zero falls back to default", "0", 1000, 1000},
		{"negative falls back to default", "-5", 1000, 1000},
		{"non-numeric falls back to default", "lots", 1000, 1000},
		{"overflowing integer falls back to default", "99999999999999999999", 1000, 1000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := httpx.MaxKeys(tc.raw, tc.def); got != tc.want {
				t.Errorf("MaxKeys(%q, %d) = %d, want %d", tc.raw, tc.def, got, tc.want)
			}
		})
	}
}

func TestMaxKeysNeverExceedsMaxPageSize(t *testing.T) {
	for _, raw := range []string{"1001", "10000", "2147483647"} {
		if got := httpx.MaxKeys(raw, 1000); got > httpx.MaxPageSize {
			t.Errorf("MaxKeys(%q) = %d, exceeds MaxPageSize %d", raw, got, httpx.MaxPageSize)
		}
	}
}
