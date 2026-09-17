package s3api

import (
	"net/http"
	"net/url"
	"slices"
)

// s3Subresources enumerates every S3 subresource query parameter Stiva is aware
// of, whether or not it implements it.
//
// This exists to close a data-loss hole: dispatch used to be an if/else chain
// that fell through to the default operation for the HTTP method whenever it did
// not recognise a subresource. That meant DELETE /<bucket>/<key>?tagging — the
// routine no-op DeleteObjectTagging that every AWS SDK may issue — deleted the
// object, and DELETE /<bucket>?tagging deleted the bucket.
//
// Any parameter in this set that reaches a handler which does not implement it
// now produces NotImplemented (501) instead of silently doing something
// destructive. Parameters *not* in this set (SDK extras, cache-busters) are
// ignored, so ordinary requests are unaffected.
var s3Subresources = map[string]struct{}{
	"accelerate":          {},
	"acl":                 {},
	"analytics":           {},
	"attributes":          {},
	"cors":                {},
	"delete":              {},
	"encryption":          {},
	"intelligent-tiering": {},
	"inventory":           {},
	"legal-hold":          {},
	"lifecycle":           {},
	"location":            {},
	"logging":             {},
	"metrics":             {},
	"notification":        {},
	"object-lock":         {},
	"ownershipControls":   {},
	"policy":              {},
	"policyStatus":        {},
	"publicAccessBlock":   {},
	"replication":         {},
	"requestPayment":      {},
	"restore":             {},
	"retention":           {},
	"select":              {},
	"tagging":             {},
	"torrent":             {},
	"uploadId":            {},
	"uploads":             {},
	"versioning":          {},
	"versions":            {},
	"website":             {},
}

// unhandledSubresource returns the name of the first known S3 subresource
// present in the query that is not listed in handled, or "" when the request
// carries no unhandled subresource.
func unhandledSubresource(query url.Values, handled ...string) string {
	for key := range query {
		if _, known := s3Subresources[key]; !known {
			continue
		}
		if slices.Contains(handled, key) {
			continue
		}
		return key
	}
	return ""
}

// rejectUnhandledSubresource writes a NotImplemented error and reports true when
// the request carries a known S3 subresource that the caller does not handle.
func rejectUnhandledSubresource(w http.ResponseWriter, query url.Values, resource string, handled ...string) bool {
	sub := unhandledSubresource(query, handled...)
	if sub == "" {
		return false
	}
	writeS3Error(w, "NotImplemented",
		"The "+sub+" subresource is not implemented by this server.", resource)
	return true
}
