package s3api

import (
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

func TestMatchOrigin(t *testing.T) {
	tests := []struct {
		origin   string
		allowed  []string
		expected bool
	}{
		{"http://example.com", []string{"*"}, true},
		{"http://example.com", []string{"http://example.com"}, true},
		{"http://example.com", []string{"https://example.com"}, false},
		{"http://sub.example.com", []string{"*.example.com"}, true},
		{"http://sub.example.com", []string{"http://*.example.com"}, true},
		{"https://sub.example.com", []string{"https://*.example.com"}, true},
		{"https://sub.example.com", []string{"http://*.example.com"}, false},
		{"https://sub.deep.example.com", []string{"https://*.example.com"}, true},
		{"http://another.com", []string{"*.example.com"}, false},
		{"http://another.com", []string{"https://*.example.com"}, false},
		{"http://sub.example.com:8080", []string{"*.example.com"}, true},
		{"http://sub.example.com:8080", []string{"http://*.example.com"}, true},
		{"https://sub.example.com:3000", []string{"https://*.example.com"}, true},
	}

	for _, tc := range tests {
		res := matchOrigin(tc.origin, tc.allowed)
		if res != tc.expected {
			t.Errorf("matchOrigin(%q, %v) = %v; want %v", tc.origin, tc.allowed, res, tc.expected)
		}
	}
}

func TestMatchMethod(t *testing.T) {
	tests := []struct {
		method   string
		allowed  []string
		expected bool
	}{
		{"GET", []string{"GET", "POST"}, true},
		{"get", []string{"GET"}, true},
		{"PUT", []string{"GET", "DELETE"}, false},
	}

	for _, tc := range tests {
		res := matchMethod(tc.method, tc.allowed)
		if res != tc.expected {
			t.Errorf("matchMethod(%q, %v) = %v; want %v", tc.method, tc.allowed, res, tc.expected)
		}
	}
}

func TestMatchHeaders(t *testing.T) {
	tests := []struct {
		reqHeaders []string
		allowed    []string
		expected   bool
	}{
		{[]string{"content-type"}, []string{"*"}, true},
		{[]string{"content-type", "x-amz-date"}, []string{"Content-Type", "x-amz-date"}, true},
		{[]string{"content-type", "authorization"}, []string{"Content-Type"}, false},
		{nil, []string{"Content-Type"}, true},
	}

	for _, tc := range tests {
		res := matchHeaders(tc.reqHeaders, tc.allowed)
		if res != tc.expected {
			t.Errorf("matchHeaders(%v, %v) = %v; want %v", tc.reqHeaders, tc.allowed, res, tc.expected)
		}
	}
}

func TestEvaluateCORS(t *testing.T) {
	cors := &storage.CORSConfiguration{
		CORSRules: []storage.CORSRule{
			{
				AllowedOrigins: []string{"http://localhost:3000"},
				AllowedMethods: []string{"GET", "PUT"},
				AllowedHeaders: []string{"*"},
				MaxAgeSeconds:  3600,
			},
		},
	}

	t.Run("Valid GET Request", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/bucket/key", nil)
		req.Header.Set("Origin", "http://localhost:3000")

		headers, matched := evaluateCORS(req, cors)
		if !matched {
			t.Fatal("Expected CORS to match")
		}

		expected := map[string]string{
			"Access-Control-Allow-Origin":  "http://localhost:3000",
			"Access-Control-Allow-Methods": "GET, PUT",
		}

		if !reflect.DeepEqual(headers, expected) {
			t.Errorf("evaluateCORS headers = %v; want %v", headers, expected)
		}
	})

	t.Run("Valid Preflight request", func(t *testing.T) {
		req := httptest.NewRequest("OPTIONS", "/bucket/key", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		req.Header.Set("Access-Control-Request-Method", "PUT")
		req.Header.Set("Access-Control-Request-Headers", "content-type, authorization")

		headers, matched := evaluateCORS(req, cors)
		if !matched {
			t.Fatal("Expected CORS to match")
		}

		expected := map[string]string{
			"Access-Control-Allow-Origin":  "http://localhost:3000",
			"Access-Control-Allow-Methods": "GET, PUT",
			"Access-Control-Allow-Headers": "content-type, authorization",
			"Access-Control-Max-Age":       "3600",
		}

		if !reflect.DeepEqual(headers, expected) {
			t.Errorf("evaluateCORS headers = %v; want %v", headers, expected)
		}
	})

	t.Run("Invalid Origin", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/bucket/key", nil)
		req.Header.Set("Origin", "http://malicious.com")

		_, matched := evaluateCORS(req, cors)
		if matched {
			t.Error("Expected CORS matching to fail")
		}
	})
}
