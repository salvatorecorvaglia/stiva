package auth

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/salvatorecorvaglia/stiva/internal/checksum"
)

const (
	amzDateHeader        = "X-Amz-Date"
	unsignedPayload      = "UNSIGNED-PAYLOAD"
	streamingPayload     = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	chunkSignaturePrefix = "AWS4-HMAC-SHA256-PAYLOAD"
	// The trailer variants carry an object checksum after the last chunk.
	// Current AWS SDKs send the unsigned one by default over HTTPS.
	streamingPayloadTrailer  = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	streamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	trailerSignaturePrefix   = "AWS4-HMAC-SHA256-TRAILER"
	trailerSignatureHeader   = "x-amz-trailer-signature"
	aws4HMACSHA256Format     = "AWS4-HMAC-SHA256\n%s\n%s\n%s"
	amzSignatureQuery        = "X-Amz-Signature"

	// maxClockSkew is the tolerated difference between the request timestamp
	// and server time, matching the AWS SigV4 window.
	maxClockSkew = 15 * time.Minute
	// maxPresignValidity is the longest lifetime a presigned URL may declare,
	// matching the S3 limit of seven days.
	maxPresignValidity = 7 * 24 * time.Hour
	// maxInMemoryPayload is the largest request body hashed entirely in memory.
	maxInMemoryPayload = 2 * 1024 * 1024
	// maxChunkSize bounds a single aws-chunked frame. The frame's declared
	// size is allocated up front to read it, and was checked only against
	// the payload limit (5GiB by default) — so one chunk header could drive a
	// 5GiB allocation before a single byte of data arrived. Real SDKs chunk
	// at 64KiB to 8MiB.
	maxChunkSize = 16 * 1024 * 1024
)

// DefaultMaxPayloadSize is the largest request body a verifier accepts unless
// SetMaxPayloadSize says otherwise: 5GiB, the S3 single-PUT maximum.
const DefaultMaxPayloadSize int64 = 5 * 1024 * 1024 * 1024

// streamingMode describes one of the aws-chunked payload encodings.
type streamingMode struct {
	// signedChunks: each chunk carries a signature chained from the request's.
	signedChunks bool
	// trailer: checksum trailer headers follow the last chunk.
	trailer bool
}

// streamingModes maps each aws-chunked X-Amz-Content-Sha256 value to its
// encoding.
var streamingModes = map[string]streamingMode{
	streamingPayload:         {signedChunks: true},
	streamingPayloadTrailer:  {signedChunks: true, trailer: true},
	streamingUnsignedTrailer: {trailer: true},
}

// AuthError represents a specific S3 authentication/signature error.
type AuthError struct {
	Code    string
	Message string
}

func (e *AuthError) Error() string {
	return e.Message
}

// maxSigningKeyCacheEntries bounds the derived-signing-key cache.
//
// The cache key is datestamp/region/service. All three come verbatim from the
// caller's credential scope, and the cache was an unbounded sync.Map that never
// evicted — so anyone who knew the access key (which is not a secret) could add
// one permanent entry per request, reached before the signature is even
// compared. Legitimate traffic needs a handful of entries at most: one region,
// one service, and two datestamps around a UTC midnight boundary.
//
// Like the bucket metadata cache in the storage layer, this drops everything at
// the bound rather than tracking recency — an entry is one HMAC chain to
// rebuild, and the bound is only a safety valve.
const maxSigningKeyCacheEntries = 64

// maxCredentialScopePart bounds a single component of the credential scope,
// which also bounds the size of a cache key built from it.
const maxCredentialScopePart = 64

// SigV4Verifier verifies AWS Signature Version 4 requests.
type SigV4Verifier struct {
	creds *Credentials
	// maxPayload bounds a request body verified against a signed payload
	// hash or decoded from aws-chunked framing. Zero means no limit. It used
	// to be the package-level MaxPayloadSize, shared by every verifier.
	maxPayload int64

	keysMu      sync.Mutex
	signingKeys map[string][]byte
}

// NewSigV4Verifier creates a new verifier with the given credentials.
func NewSigV4Verifier(creds *Credentials) *SigV4Verifier {
	return &SigV4Verifier{
		creds:       creds,
		maxPayload:  DefaultMaxPayloadSize,
		signingKeys: make(map[string][]byte, maxSigningKeyCacheEntries),
	}
}

// SetMaxPayloadSize sets the largest request body the verifier accepts. Zero
// disables the limit. Call it before the verifier serves requests.
func (v *SigV4Verifier) SetMaxPayloadSize(n int64) {
	v.maxPayload = n
}

// validateCredentialScope rejects a credential scope whose region or service
// could not have come from a real S3 client. Beyond being more correct than
// accepting anything, it keeps the signing-key cache key space small.
func validateCredentialScope(region, service string) error {
	if service != "s3" {
		return &AuthError{Code: "AuthorizationHeaderMalformed", Message: "Credential should be scoped to the service 's3'."}
	}
	if region == "" || len(region) > maxCredentialScopePart {
		return &AuthError{Code: "AuthorizationHeaderMalformed", Message: "Credential contains an invalid region."}
	}
	return nil
}

// Creds returns the credentials associated with this verifier.
func (v *SigV4Verifier) Creds() *Credentials {
	return v.creds
}

// parsedAuth holds the components parsed from the Authorization header.
type parsedAuth struct {
	Algorithm     string
	Credential    string
	AccessKey     string
	Date          string
	Region        string
	Service       string
	SignedHeaders []string
	Signature     string
}

// Verify checks the AWS SigV4 signature on an HTTP request.
// Returns nil if the signature is valid, or an error describing the failure.
func (v *SigV4Verifier) Verify(r *http.Request) error {
	// Check for query string authentication (presigned URLs)
	if r.URL.Query().Get("X-Amz-Algorithm") != "" {
		return v.verifyPresigned(r)
	}

	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return &AuthError{Code: "AccessDenied", Message: "missing Authorization header"}
	}

	parsed, err := parseAuthHeader(authHeader)
	if err != nil {
		return &AuthError{Code: "AccessDenied", Message: err.Error()}
	}

	if parsed.AccessKey != v.creds.AccessKey {
		return &AuthError{Code: "InvalidAccessKeyId", Message: "The Access Key Id you provided does not exist in our records."}
	}

	if err := validateCredentialScope(parsed.Region, parsed.Service); err != nil {
		return err
	}
	if err := requireSignedHost(parsed.SignedHeaders); err != nil {
		return err
	}

	// Get the date for signing
	dateStr := r.Header.Get(amzDateHeader)
	if dateStr == "" {
		dateStr = r.Header.Get("Date")
	}
	if dateStr == "" {
		return &AuthError{Code: "AccessDenied", Message: "missing date header"}
	}

	// Parse the date
	t, err := time.Parse("20060102T150405Z", dateStr)
	if err != nil {
		// Try RFC2616 format
		t, err = time.Parse(time.RFC1123, dateStr)
		if err != nil {
			return &AuthError{Code: "AccessDenied", Message: "invalid date format"}
		}
	}

	// Check for date/time skew (standard is 15 minutes)
	if time.Now().UTC().Sub(t).Abs() > maxClockSkew {
		return &AuthError{Code: "RequestTimeTooSkewed", Message: "The difference between the request time and the current time is too large."}
	}

	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	mode, isStreaming := streamingModes[payloadHash]

	datestamp := t.Format("20060102")

	// Reconstruct canonical request
	canonicalRequest := v.buildCanonicalRequest(r, parsed.SignedHeaders)

	// Build string to sign
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, parsed.Region, parsed.Service)
	stringToSign := fmt.Sprintf(aws4HMACSHA256Format,
		dateStr,
		scope,
		hashSHA256([]byte(canonicalRequest)),
	)

	// Calculate expected signature
	signingKey := v.deriveSigningKey(datestamp, parsed.Region, parsed.Service)
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expectedSig), []byte(parsed.Signature)) {
		return &AuthError{Code: "SignatureDoesNotMatch", Message: "The request signature we calculated does not match the signature you provided."}
	}

	// Only now that the signature checks out do we touch the body. Hashing it
	// first meant an unauthenticated caller — the access key is not a secret —
	// could make the server spool up to the payload limit to disk per request and
	// only then be told the signature was wrong. The canonical request above
	// uses the *header* value, so verifying in this order changes nothing
	// about what is signed.
	if payloadHash != "" && payloadHash != unsignedPayload && !isStreaming {
		if err := verifySignedPayload(r, payloadHash, v.maxPayload); err != nil {
			return err
		}
	}

	// The STREAMING-* modes carry the body as aws-chunked frames: signed and
	// chained from the request's own signature above, or unsigned with a
	// checksum trailer. The header signature alone says nothing about the
	// body, so r.Body is replaced with a decoder that verifies each chunk as
	// it is read — otherwise the chunk framing would be written to disk as if
	// it were the object's content.
	if isStreaming {
		if err := decodeStreamingPayload(r, mode, signingKey, dateStr, scope, expectedSig, v.maxPayload); err != nil {
			return err
		}
	}

	return nil
}

// verifyPresigned handles presigned URL verification.
func (v *SigV4Verifier) verifyPresigned(r *http.Request) error {
	q := r.URL.Query()

	credential := q.Get("X-Amz-Credential")
	parts := strings.Split(credential, "/")
	if len(parts) != 5 {
		return &AuthError{Code: "AccessDenied", Message: "invalid credential format"}
	}

	accessKey := parts[0]
	datestamp := parts[1]
	region := parts[2]
	service := parts[3]

	if accessKey != v.creds.AccessKey {
		return &AuthError{Code: "InvalidAccessKeyId", Message: "The Access Key Id you provided does not exist in our records."}
	}

	// The presigned credential scope is entirely caller-supplied, including the
	// datestamp, and feeds the signing-key cache key.
	if err := validateCredentialScope(region, service); err != nil {
		return err
	}
	if len(datestamp) > maxCredentialScopePart {
		return &AuthError{Code: "AccessDenied", Message: "invalid credential format"}
	}

	dateStr := q.Get(amzDateHeader)
	signedHeaders := strings.Split(q.Get("X-Amz-SignedHeaders"), ";")
	signature := q.Get(amzSignatureQuery)
	if err := requireSignedHost(signedHeaders); err != nil {
		return err
	}

	// Expiry is mandatory. It used to be checked only when X-Amz-Expires was
	// present, so a URL signed without that parameter never expired at all.
	signedAt, err := time.Parse("20060102T150405Z", dateStr)
	if err != nil {
		return &AuthError{Code: "AccessDenied", Message: "invalid or missing X-Amz-Date"}
	}

	expires := q.Get("X-Amz-Expires")
	if expires == "" {
		return &AuthError{Code: "AccessDenied", Message: "X-Amz-Expires is required for presigned requests"}
	}

	dur, err := strconv.Atoi(expires)
	if err != nil || dur <= 0 {
		return &AuthError{Code: "AccessDenied", Message: "invalid expires value"}
	}
	if time.Duration(dur)*time.Second > maxPresignValidity {
		return &AuthError{
			Code:    "AccessDenied",
			Message: "X-Amz-Expires exceeds the maximum allowed value of 604800 seconds",
		}
	}

	now := time.Now().UTC()
	if now.After(signedAt.Add(time.Duration(dur) * time.Second)) {
		return &AuthError{Code: "AccessDenied", Message: "Request has expired"}
	}
	// Reject URLs dated far in the future, which would otherwise extend the
	// effective lifetime well beyond the stated expiry window.
	if signedAt.After(now.Add(maxClockSkew)) {
		return &AuthError{
			Code:    "RequestTimeTooSkewed",
			Message: "The difference between the request time and the current time is too large.",
		}
	}

	// Build canonical request for presigned URL
	// For presigned, the payload hash is unsignedPayload
	canonicalRequest := v.buildCanonicalRequestPresigned(r, signedHeaders)

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", datestamp, region, service)
	stringToSign := fmt.Sprintf(aws4HMACSHA256Format,
		dateStr,
		scope,
		hashSHA256([]byte(canonicalRequest)),
	)

	signingKey := v.deriveSigningKey(datestamp, region, service)
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	if !hmac.Equal([]byte(expectedSig), []byte(signature)) {
		return &AuthError{Code: "SignatureDoesNotMatch", Message: "The request signature we calculated does not match the signature you provided."}
	}

	return nil
}

// requireSignedHost rejects a signature that does not cover the Host header.
// SigV4 requires it, and with virtual-host routing the bucket is in Host: a
// captured request signed without it could be replayed against another bucket.
func requireSignedHost(signedHeaders []string) error {
	for _, h := range signedHeaders {
		if strings.EqualFold(strings.TrimSpace(h), "host") {
			return nil
		}
	}
	return &AuthError{Code: "AccessDenied", Message: "The Host header must be included in the signed headers."}
}

// AccessKeyFromAuthHeader returns the access key named in a SigV4
// Authorization header, or "" if the header is absent or unparseable.
//
// The access logger needs exactly this and used to re-implement the parsing
// inline, splitting on commas and slashes in its own slightly different way.
func AccessKeyFromAuthHeader(header string) string {
	parsed, err := parseAuthHeader(header)
	if err != nil {
		return ""
	}
	return parsed.AccessKey
}

// parseAuthHeader parses an AWS SigV4 Authorization header.
// Format: AWS4-HMAC-SHA256 Credential=<accessKey>/<date>/<region>/<service>/aws4_request, SignedHeaders=<headers>, Signature=<sig>
func parseAuthHeader(header string) (*parsedAuth, error) {
	if !strings.HasPrefix(header, "AWS4-HMAC-SHA256 ") {
		return nil, fmt.Errorf("unsupported auth algorithm")
	}

	content := strings.TrimPrefix(header, "AWS4-HMAC-SHA256 ")
	parts := strings.Split(content, ", ")

	parsed := &parsedAuth{Algorithm: "AWS4-HMAC-SHA256"}

	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		value := strings.TrimSpace(kv[1])

		switch key {
		case "Credential":
			parsed.Credential = value
			credParts := strings.Split(value, "/")
			if len(credParts) >= 5 {
				parsed.AccessKey = credParts[0]
				parsed.Date = credParts[1]
				parsed.Region = credParts[2]
				parsed.Service = credParts[3]
			}
		case "SignedHeaders":
			parsed.SignedHeaders = strings.Split(value, ";")
		case "Signature":
			parsed.Signature = value
		}
	}

	if parsed.AccessKey == "" || parsed.Signature == "" {
		return nil, fmt.Errorf("incomplete authorization header")
	}

	return parsed, nil
}

// buildCanonicalRequest constructs the canonical request string for SigV4.
func (v *SigV4Verifier) buildCanonicalRequest(r *http.Request, signedHeaders []string) string {
	// HTTP method
	method := r.Method

	// Canonical URI (URL-encoded path)
	canonicalURI := getCanonicalURI(r.URL)

	// Canonical query string
	canonicalQueryString := getCanonicalQueryString(r.URL.Query())

	// Canonical headers
	sort.Strings(signedHeaders)
	var canonicalHeaders strings.Builder
	for _, h := range signedHeaders {
		h = strings.ToLower(strings.TrimSpace(h))
		value := strings.TrimSpace(r.Header.Get(h))
		if h == "host" && value == "" {
			value = r.Host
		}
		canonicalHeaders.WriteString(h)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(value)
		canonicalHeaders.WriteString("\n")
	}

	signedHeadersStr := strings.Join(signedHeaders, ";")

	// Payload hash
	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = unsignedPayload
	}

	return fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders.String(),
		signedHeadersStr,
		payloadHash,
	)
}

// buildCanonicalRequestPresigned constructs the canonical request for presigned URLs.
func (v *SigV4Verifier) buildCanonicalRequestPresigned(r *http.Request, signedHeaders []string) string {
	method := r.Method
	canonicalURI := getCanonicalURI(r.URL)

	// For presigned URLs, exclude X-Amz-Signature from query string
	queryParams := r.URL.Query()
	queryParams.Del(amzSignatureQuery)
	canonicalQueryString := getCanonicalQueryString(queryParams)

	sort.Strings(signedHeaders)
	var canonicalHeaders strings.Builder
	for _, h := range signedHeaders {
		h = strings.ToLower(strings.TrimSpace(h))
		value := strings.TrimSpace(r.Header.Get(h))
		if h == "host" && value == "" {
			value = r.Host
		}
		canonicalHeaders.WriteString(h)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(value)
		canonicalHeaders.WriteString("\n")
	}

	signedHeadersStr := strings.Join(signedHeaders, ";")

	return fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		method,
		canonicalURI,
		canonicalQueryString,
		canonicalHeaders.String(),
		signedHeadersStr,
		unsignedPayload,
	)
}

// getCanonicalURI returns the URI-encoded path.
func getCanonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return path
}

func awsPercentEncode(s string) string {
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '~' || c == '.' {
			buf.WriteByte(c)
		} else {
			fmt.Fprintf(&buf, "%%%02X", c)
		}
	}
	return buf.String()
}

// getCanonicalQueryString returns the sorted, URL-encoded query string.
func getCanonicalQueryString(values url.Values) string {
	if len(values) == 0 {
		return ""
	}

	var keys []string
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var pairs []string
	for _, k := range keys {
		vs := values[k]
		sort.Strings(vs)
		for _, v := range vs {
			pairs = append(pairs, awsPercentEncode(k)+"="+awsPercentEncode(v))
		}
	}

	return strings.Join(pairs, "&")
}

// deriveSigningKey derives the SigV4 signing key.
func (v *SigV4Verifier) deriveSigningKey(datestamp, region, service string) []byte {
	cacheKey := datestamp + "/" + region + "/" + service

	v.keysMu.Lock()
	if key, ok := v.signingKeys[cacheKey]; ok {
		v.keysMu.Unlock()
		return key
	}
	v.keysMu.Unlock()

	kDate := hmacSHA256([]byte("AWS4"+v.creds.SecretKey), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))

	v.keysMu.Lock()
	if v.signingKeys == nil || len(v.signingKeys) >= maxSigningKeyCacheEntries {
		v.signingKeys = make(map[string][]byte, maxSigningKeyCacheEntries)
	}
	v.signingKeys[cacheKey] = kSigning
	v.keysMu.Unlock()

	return kSigning
}

// hmacSHA256 computes HMAC-SHA256.
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(data)
	return h.Sum(nil)
}

// hashSHA256 computes SHA256 hash and returns hex string.
func hashSHA256(data []byte) string {
	h := sha256.New()
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// deferredBody is a request body whose integrity is checked as it is read:
// a problem surfaces as a read error before the end of the body, never as a
// clean io.EOF. That suits a consumer that reads to the end and only then
// commits (an object or part upload), but not one that may stop early, such
// as an XML decoder; see IsDeferred.
type deferredBody struct {
	io.Reader
	closer io.Closer
}

func (d *deferredBody) Close() error {
	if d.closer == nil {
		return nil
	}
	return d.closer.Close()
}

// IsDeferred reports whether body is still being verified as it is read (a
// large signed payload, or an aws-chunked stream). A caller that might not
// read it to the end must read it fully, and check for errors, before acting
// on any of it.
func IsDeferred(body io.Reader) bool {
	_, ok := body.(*deferredBody)
	return ok
}

// payloadHashMismatch is the error for a body that does not match its signed
// X-Amz-Content-Sha256.
func payloadHashMismatch() error {
	return &AuthError{
		Code:    "XAmzContentSHA256Mismatch",
		Message: "The provided 'x-amz-content-sha256' header does not match what was computed.",
	}
}

// verifySignedPayload checks the body against the signed SHA-256 in
// X-Amz-Content-Sha256.
//
// A body that fits in memory is read and checked here, as before. A larger
// one used to be spooled whole to a temp file to be hashed and then copied
// again into the object — every signed upload written to disk twice, with a
// full extra copy on disk meanwhile. It is now wrapped so the hash is computed
// as the body streams through, and a mismatch is reported in place of io.EOF.
func verifySignedPayload(r *http.Request, expectedHex string, maxPayload int64) error {
	want, err := hex.DecodeString(expectedHex)
	if err != nil || len(want) != sha256.Size {
		return &AuthError{Code: "InvalidArgument", Message: "x-amz-content-sha256 must be UNSIGNED-PAYLOAD, a STREAMING-* value, or a SHA-256 hex digest."}
	}

	if r.Body == nil {
		if !bytes.Equal(want, emptySHA256[:]) {
			return payloadHashMismatch()
		}
		return nil
	}

	// Reject oversized bodies up front when the length is declared.
	if maxPayload > 0 && r.ContentLength > maxPayload {
		return entityTooLarge()
	}

	head, err := io.ReadAll(io.LimitReader(r.Body, maxInMemoryPayload+1))
	if err != nil {
		return err
	}
	if len(head) <= maxInMemoryPayload {
		sum := sha256.Sum256(head)
		if !bytes.Equal(sum[:], want) {
			return payloadHashMismatch()
		}
		r.Body = io.NopCloser(bytes.NewReader(head))
		return nil
	}

	// r.ContentLength is -1 for Transfer-Encoding: chunked, so the check above
	// does not fire for it; cap the stream itself as well.
	rest := io.MultiReader(bytes.NewReader(head), r.Body)
	if maxPayload > 0 {
		rest = &cappedReader{r: rest, remaining: maxPayload}
	}
	r.Body = &deferredBody{
		Reader: checksum.NewVerifyingReader(rest, sha256.New(), want, payloadHashMismatch()),
		closer: r.Body,
	}
	return nil
}

var emptySHA256 = sha256.Sum256(nil)

func entityTooLarge() error {
	return &AuthError{Code: "EntityTooLarge", Message: "Your proposed upload exceeds the maximum allowed size."}
}

// cappedReader fails with EntityTooLarge once more than remaining bytes have
// been read, rather than silently truncating like io.LimitReader.
type cappedReader struct {
	r         io.Reader
	remaining int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	if c.remaining < 0 {
		return 0, entityTooLarge()
	}
	return n, err
}

// emptyPayloadHash is the SHA256 hash of an empty string, used by the
// aws-chunked chunk-signing string-to-sign as a stand-in for "hash of signed
// trailer headers" when the request carries none.
var emptyPayloadHash = hashSHA256(nil)

// maxChunkLineLength bounds an aws-chunked chunk header or trailer line.
// Real ones are well under 200 bytes; the old unbounded ReadString let a
// client stream an endless "line" into memory.
const maxChunkLineLength = 4096

// errChunkLine reports a chunk header or trailer line that is too long or cut
// off by the end of the body.
var errChunkLine = fmt.Errorf("malformed chunk line")

// readChunkLine reads one CRLF-terminated line of aws-chunked framing. It
// returns io.EOF only if the body ended cleanly before any byte of a line.
func readChunkLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return "", io.EOF
		}
		return "", errChunkLine
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// declaredChecksumTrailers parses X-Amz-Trailer, the (signed) list of
// trailers the client will send. Only S3 checksum trailers are defined.
func declaredChecksumTrailers(header string) (map[string]hash.Hash, error) {
	hashes := make(map[string]hash.Hash)
	if strings.TrimSpace(header) == "" {
		return hashes, nil
	}
	for _, name := range strings.Split(header, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		h, ok := checksum.New(name)
		if !ok {
			return nil, &AuthError{Code: "InvalidRequest", Message: fmt.Sprintf("The trailer %q is not supported.", name)}
		}
		hashes[name] = h
	}
	return hashes, nil
}

// decodeStreamingPayload replaces r.Body with a decoder for an aws-chunked
// body, verifying it as the mode requires while it is read.
//
// Wire format: chunks of "<hex-size>[;chunk-signature=<sig>]\r\n<data>\r\n",
// then a zero-size chunk, then any trailer lines ("name:value\r\n", with a
// final x-amz-trailer-signature line in the signed trailer mode), then an
// empty line. In the signed modes each chunk's signature, chained from the
// request's own Authorization signature, is checked before any of its data is
// returned. A checksum trailer and x-amz-decoded-content-length are checked at
// the end, and a failure is returned in place of io.EOF.
//
// The body used to be decoded in full, and spooled to a temp file past 2MiB,
// before the handler saw a byte; now one chunk is held in memory at a time.
func decodeStreamingPayload(r *http.Request, mode streamingMode, signingKey []byte, dateStr, scope, seedSignature string, maxPayload int64) error {
	if r.Body == nil {
		return nil
	}

	cr := &chunkedReader{
		br:            bufio.NewReaderSize(r.Body, maxChunkLineLength),
		mode:          mode,
		signingKey:    signingKey,
		dateStr:       dateStr,
		scope:         scope,
		prevSignature: seedSignature,
		declaredLen:   -1,
		maxPayload:    maxPayload,
	}
	if mode.trailer {
		var err error
		if cr.trailerHashes, err = declaredChecksumTrailers(r.Header.Get("X-Amz-Trailer")); err != nil {
			return err
		}
	}
	if raw := r.Header.Get("X-Amz-Decoded-Content-Length"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return &AuthError{Code: "InvalidArgument", Message: "Invalid x-amz-decoded-content-length."}
		}
		if maxPayload > 0 && n > maxPayload {
			return entityTooLarge()
		}
		cr.declaredLen = n
	}

	r.Body = &deferredBody{Reader: cr, closer: r.Body}
	// The decoded size, which storage checks the written length against.
	r.ContentLength = cr.declaredLen
	return nil
}

// chunkedReader decodes and verifies an aws-chunked body as it is read.
type chunkedReader struct {
	br            *bufio.Reader
	mode          streamingMode
	signingKey    []byte
	dateStr       string
	scope         string
	prevSignature string
	trailerHashes map[string]hash.Hash
	declaredLen   int64 // x-amz-decoded-content-length, or -1
	maxPayload    int64 // zero: no limit

	buf     []byte // reused chunk buffer
	pending []byte // verified data not yet returned
	decoded int64
	err     error // sticky: io.EOF once fully verified, or the failure
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	for len(c.pending) == 0 {
		if c.err != nil {
			return 0, c.err
		}
		c.err = c.nextChunk()
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// malformed reports a framing error: a signature failure in the signed modes
// (the body does not match what was signed), a bad request in the unsigned one.
func (c *chunkedReader) malformed(msg string) error {
	code := "InvalidRequest"
	if c.mode.signedChunks {
		code = "SignatureDoesNotMatch"
	}
	return &AuthError{Code: code, Message: "malformed chunked payload: " + msg}
}

// nextChunk reads and verifies one chunk, leaving its data in pending. After
// the final zero-size chunk it verifies the trailer section and returns
// io.EOF, or the verification failure.
func (c *chunkedReader) nextChunk() error {
	header, err := readChunkLine(c.br)
	if err != nil {
		return c.malformed("could not read chunk header")
	}

	sizeStr, ext, _ := strings.Cut(header, ";")
	declaredSig := ""
	if c.mode.signedChunks {
		sig, ok := strings.CutPrefix(ext, "chunk-signature=")
		if !ok {
			return c.malformed("missing chunk-signature")
		}
		declaredSig = sig
	}
	chunkSize, err := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
	if err != nil || chunkSize < 0 {
		return c.malformed("invalid chunk size")
	}
	if c.maxPayload > 0 && c.decoded+chunkSize > c.maxPayload {
		return entityTooLarge()
	}
	// Bound the allocation below, which happens before any data is read.
	if chunkSize > maxChunkSize {
		return &AuthError{Code: "InvalidRequest", Message: "Chunk size exceeds the maximum allowed for an aws-chunked upload."}
	}

	if int64(cap(c.buf)) < chunkSize {
		c.buf = make([]byte, chunkSize)
	}
	data := c.buf[:chunkSize]
	if chunkSize > 0 {
		if _, err := io.ReadFull(c.br, data); err != nil {
			return c.malformed("truncated chunk data")
		}
		if terminator, err := readChunkLine(c.br); err != nil || terminator != "" {
			return c.malformed("missing chunk terminator")
		}
	}

	if c.mode.signedChunks {
		stringToSign := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
			chunkSignaturePrefix, c.dateStr, c.scope, c.prevSignature, emptyPayloadHash, hashSHA256(data))
		expectedSig := hex.EncodeToString(hmacSHA256(c.signingKey, []byte(stringToSign)))
		if !hmac.Equal([]byte(expectedSig), []byte(declaredSig)) {
			return &AuthError{Code: "SignatureDoesNotMatch", Message: "chunk signature does not match"}
		}
		c.prevSignature = expectedSig
	}

	if chunkSize == 0 {
		return c.finish()
	}
	for _, h := range c.trailerHashes {
		_, _ = h.Write(data)
	}
	c.decoded += chunkSize
	c.pending = data
	return nil
}

// finish reads and verifies the trailer section that follows the last chunk:
// zero or more "name:value" lines, then an empty line. Without a trailer
// mode it is just the empty line; a body that ends cleanly instead of sending
// it is tolerated. It returns io.EOF when everything checks out.
func (c *chunkedReader) finish() error {
	trailerValues := make(map[string]string)
	var canonicalTrailers strings.Builder
	trailerSig := ""
	for {
		line, err := readChunkLine(c.br)
		if errors.Is(err, io.EOF) || (err == nil && line == "") {
			break
		}
		if err != nil {
			return c.malformed("could not read trailer")
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return c.malformed("invalid trailer line")
		}
		name, value = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(value)

		if name == trailerSignatureHeader && c.mode.signedChunks && c.mode.trailer && trailerSig == "" {
			trailerSig = value
			continue
		}
		switch {
		case !c.mode.trailer:
			return c.malformed("unexpected trailer")
		case trailerSig != "":
			return c.malformed("trailer after the trailer signature")
		case c.trailerHashes[name] == nil:
			return &AuthError{Code: "InvalidRequest", Message: fmt.Sprintf("The trailer %q was not declared in x-amz-trailer.", name)}
		case trailerValues[name] != "":
			return c.malformed("duplicate trailer")
		}
		trailerValues[name] = value
		canonicalTrailers.WriteString(name + ":" + value + "\n")
	}

	if c.mode.signedChunks && c.mode.trailer {
		stringToSign := fmt.Sprintf("%s\n%s\n%s\n%s\n%s",
			trailerSignaturePrefix, c.dateStr, c.scope, c.prevSignature, hashSHA256([]byte(canonicalTrailers.String())))
		expectedSig := hex.EncodeToString(hmacSHA256(c.signingKey, []byte(stringToSign)))
		if !hmac.Equal([]byte(expectedSig), []byte(trailerSig)) {
			return &AuthError{Code: "SignatureDoesNotMatch", Message: "trailer signature does not match"}
		}
	}

	for name, h := range c.trailerHashes {
		raw, ok := trailerValues[name]
		if !ok {
			return &AuthError{Code: "InvalidRequest", Message: fmt.Sprintf("The declared trailer %q was not sent.", name)}
		}
		want, err := checksum.DecodeExpected(raw, h)
		if err != nil {
			return &AuthError{Code: "InvalidRequest", Message: fmt.Sprintf("Invalid %s trailer: %v.", name, err)}
		}
		if !bytes.Equal(h.Sum(nil), want) {
			return &AuthError{Code: "BadDigest", Message: fmt.Sprintf("The %s you specified did not match the calculated checksum.", name)}
		}
	}

	if c.declaredLen >= 0 && c.declaredLen != c.decoded {
		return &AuthError{Code: "IncompleteBody", Message: "The decoded payload length does not match x-amz-decoded-content-length."}
	}
	return io.EOF
}

// PresignGetObject generates a presigned GET URL for S3 compatibility.
// extraParams carries additional query parameters to include in the signed
// URL, such as the "response-content-disposition" family of response-header
// overrides. It may be nil.
func PresignGetObject(accessKey, secretKey, region, bucket, key string, expires time.Duration, s3Endpoint string, extraParams url.Values) (string, error) {
	t := time.Now().UTC()
	datestamp := t.Format("20060102")
	amzDate := t.Format("20060102T150405Z")
	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", datestamp, region)

	u, err := url.Parse(s3Endpoint)
	if err != nil {
		return "", err
	}
	host := u.Host

	query := url.Values{}
	for k, vs := range extraParams {
		for _, v := range vs {
			query.Add(k, v)
		}
	}
	query.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	query.Set("X-Amz-Credential", accessKey+"/"+credentialScope)
	query.Set(amzDateHeader, amzDate)
	query.Set("X-Amz-Expires", fmt.Sprintf("%.0f", expires.Seconds()))
	query.Set("X-Amz-SignedHeaders", "host")

	cleanKey := strings.TrimPrefix(key, "/")
	escapedPath := ""
	for _, part := range strings.Split(cleanKey, "/") {
		if escapedPath != "" {
			escapedPath += "/"
		}
		escapedPath += url.PathEscape(part)
	}

	canonicalPath := "/" + bucket + "/" + escapedPath

	// Must be the exact encoding the verifier recomputes. This used
	// url.QueryEscape, which writes a space as "+" where SigV4 requires "%20",
	// so any parameter containing a space (such as the
	// response-content-disposition on every console share link) produced a
	// URL that could never verify.
	canonicalQuery := getCanonicalQueryString(query)

	// Canonical headers
	canonicalHeaders := "host:" + host + "\n"
	signedHeaders := "host"

	// Canonical request
	canonicalRequest := fmt.Sprintf("GET\n%s\n%s\n%s\n%s\n"+unsignedPayload,
		canonicalPath,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
	)

	// String to sign
	stringToSign := fmt.Sprintf(aws4HMACSHA256Format,
		amzDate,
		credentialScope,
		hashSHA256([]byte(canonicalRequest)),
	)

	// Signing key derivation
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))

	// Calculate signature
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	endpoint := strings.TrimSuffix(s3Endpoint, "/")

	// Emit the query in its canonical form too, so the URL carries exactly the
	// bytes that were signed rather than relying on the receiver to decode a
	// differently-escaped form back to the same values.
	return endpoint + canonicalPath + "?" + canonicalQuery + "&" + amzSignatureQuery + "=" + signature, nil
}

// SignRequest signs an HTTP request with AWS Signature Version 4.
func SignRequest(req *http.Request, accessKey, secretKey, region, service string) {
	t := time.Now().UTC()
	dateBasic := t.Format("20060102T150405Z")
	dateDay := t.Format("20060102")

	req.Header.Set("X-Amz-Date", dateBasic)

	// Set Host header
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	if host == "" {
		host = "localhost"
	}
	req.Header.Set("Host", host)

	// For S3 compatibility with large stream requests, we sign with UNSIGNED-PAYLOAD.
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	hashedPayload := "UNSIGNED-PAYLOAD"

	// Canonical headers to sign
	headers := []string{"host", "x-amz-date", "x-amz-content-sha256"}
	if req.Header.Get("Content-Type") != "" {
		headers = append(headers, "content-type")
	}
	sort.Strings(headers)

	var canonicalHeaders strings.Builder
	for _, h := range headers {
		canonicalHeaders.WriteString(h)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(strings.TrimSpace(req.Header.Get(h)))
		canonicalHeaders.WriteString("\n")
	}

	signedHeaders := strings.Join(headers, ";")

	// Escaping path correctly
	escapedPath := req.URL.EscapedPath()
	if escapedPath == "" {
		escapedPath = "/"
	}

	canonicalQuery := req.URL.Query().Encode()

	// Canonical Request
	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		req.Method,
		escapedPath,
		canonicalQuery,
		canonicalHeaders.String(),
		signedHeaders,
		hashedPayload,
	)

	hReq := sha256.Sum256([]byte(canonicalRequest))
	hashedCanonicalRequest := hex.EncodeToString(hReq[:])

	// Credential Scope
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateDay, region, service)

	// String to Sign
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		dateBasic,
		credentialScope,
		hashedCanonicalRequest,
	)

	// Deriving Key
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(dateDay))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))

	// Signature
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	// Auth Header
	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey,
		credentialScope,
		signedHeaders,
		signature,
	)

	req.Header.Set("Authorization", authHeader)
}
