package s3api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// evaluateCORS evaluates a request against a bucket's CORS configuration.
// If a rule matches, it returns a map of CORS headers that should be set, and true.
// Otherwise, it returns nil, false.
func evaluateCORS(r *http.Request, cors *storage.CORSConfiguration) (map[string]string, bool) {
	if cors == nil || len(cors.CORSRules) == 0 {
		return nil, false
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil, false
	}

	var requestMethod string
	if r.Method == http.MethodOptions {
		requestMethod = r.Header.Get("Access-Control-Request-Method")
	} else {
		requestMethod = r.Method
	}
	if requestMethod == "" {
		return nil, false
	}

	var requestHeaders []string
	if r.Method == http.MethodOptions {
		reqHeadersStr := r.Header.Get("Access-Control-Request-Headers")
		if reqHeadersStr != "" {
			for _, h := range strings.Split(reqHeadersStr, ",") {
				requestHeaders = append(requestHeaders, strings.ToLower(strings.TrimSpace(h)))
			}
		}
	}

	for _, rule := range cors.CORSRules {
		if !matchOrigin(origin, rule.AllowedOrigins) {
			continue
		}

		if !matchMethod(requestMethod, rule.AllowedMethods) {
			continue
		}

		if r.Method == http.MethodOptions && !matchHeaders(requestHeaders, rule.AllowedHeaders) {
			continue
		}

		// Found a matching rule! Prepare response headers.
		//
		// Access-Control-Allow-Credentials is deliberately NOT set. S3 never
		// sets it, and emitting it for every non-wildcard rule silently turned
		// a plain origin allowlist into permission for those origins to make
		// credentialed cross-origin requests.
		headers := make(map[string]string)
		headers["Access-Control-Allow-Origin"] = origin
		headers["Access-Control-Allow-Methods"] = strings.Join(rule.AllowedMethods, ", ")

		if r.Method == http.MethodOptions {
			// Preflight-specific headers
			reqHeadersStr := r.Header.Get("Access-Control-Request-Headers")
			if reqHeadersStr != "" {
				headers["Access-Control-Allow-Headers"] = reqHeadersStr
			}
			if rule.MaxAgeSeconds > 0 {
				headers["Access-Control-Max-Age"] = strconv.Itoa(rule.MaxAgeSeconds)
			}
		} else if len(rule.ExposeHeaders) > 0 {
			// Normal request specific headers
			headers["Access-Control-Expose-Headers"] = strings.Join(rule.ExposeHeaders, ", ")
		}

		return headers, true
	}

	return nil, false
}

func matchOrigin(origin string, allowedOrigins []string) bool {
	origin = strings.ToLower(origin)
	originURL, err := url.Parse(origin)
	hasParsedOrigin := (err == nil && originURL.Host != "")

	for _, allowed := range allowedOrigins {
		if allowed == "*" {
			return true
		}
		if strings.ToLower(allowed) == origin {
			return true
		}

		// Support wildcard matches with schemes (e.g., https://*.example.com)
		if hasParsedOrigin {
			allowedURL, err := url.Parse(strings.ToLower(allowed))
			if err == nil && allowedURL.Host != "" {
				if originURL.Scheme == allowedURL.Scheme {
					if strings.HasPrefix(allowedURL.Host, "*.") {
						suffix := allowedURL.Host[1:] // e.g. .example.com
						if strings.HasSuffix(originURL.Hostname(), suffix) {
							return true
						}
					}
				}
			}
		}

		// Fallback: Support simple subdomain wildcard match (e.g. *.example.com) without scheme
		if strings.HasPrefix(allowed, "*.") {
			suffix := strings.ToLower(allowed[1:]) // e.g. .example.com
			if hasParsedOrigin {
				if strings.HasSuffix(originURL.Hostname(), suffix) {
					return true
				}
			} else {
				if strings.HasSuffix(origin, suffix) {
					return true
				}
			}
		}
	}
	return false
}

func matchMethod(method string, allowedMethods []string) bool {
	method = strings.ToUpper(method)
	for _, allowed := range allowedMethods {
		if strings.ToUpper(allowed) == method {
			return true
		}
	}
	return false
}

func matchHeaders(reqHeaders, allowedHeaders []string) bool {
	if len(reqHeaders) == 0 {
		return true
	}
	// Check if all requested headers are allowed
	for _, reqH := range reqHeaders {
		allowed := false
		for _, allowedH := range allowedHeaders {
			if allowedH == "*" {
				allowed = true
				break
			}
			if strings.ToLower(allowedH) == reqH {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}
