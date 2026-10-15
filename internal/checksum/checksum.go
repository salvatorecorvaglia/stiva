// Package checksum implements the object checksums S3 clients send, either as
// request headers (x-amz-checksum-*, Content-MD5) or as aws-chunked trailers,
// so the server can verify an upload arrived intact before storing it.
package checksum

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // SHA-1 is one of the checksum algorithms the S3 API defines; used for integrity, not security
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
)

// crc64NVMETable is CRC-64/NVME (the S3 "CRC64NVME" algorithm): polynomial
// 0xAD93D23594C93659 in the reflected form hash/crc64 expects. crc64 applies
// the all-ones initial value and final XOR that this CRC specifies.
var crc64NVMETable = crc64.MakeTable(0x9A6C9329AC4BC9B5)

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// New returns a hash for the given lower-case checksum header or trailer name
// (for example "x-amz-checksum-crc32"), and false if S3 defines no such
// algorithm.
func New(name string) (hash.Hash, bool) {
	switch name {
	case "x-amz-checksum-crc32":
		return crc32.NewIEEE(), true
	case "x-amz-checksum-crc32c":
		return crc32.New(crc32cTable), true
	case "x-amz-checksum-crc64nvme":
		return crc64.New(crc64NVMETable), true
	case "x-amz-checksum-sha1":
		return sha1.New(), true //nolint:gosec // see import
	case "x-amz-checksum-sha256":
		return sha256.New(), true
	}
	return nil, false
}

// Names lists every checksum header name New accepts.
var Names = []string{
	"x-amz-checksum-crc32",
	"x-amz-checksum-crc32c",
	"x-amz-checksum-crc64nvme",
	"x-amz-checksum-sha1",
	"x-amz-checksum-sha256",
}

// DecodeExpected decodes a base64 checksum value and checks it has the length
// h produces.
func DecodeExpected(raw string, h hash.Hash) ([]byte, error) {
	want, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("checksum value is not valid base64")
	}
	if len(want) != h.Size() {
		return nil, fmt.Errorf("checksum value is %d bytes, want %d", len(want), h.Size())
	}
	return want, nil
}

// NewVerifyingReader passes r through while hashing it with h. At the end of
// r it compares the digest with want and, if they differ, returns mismatch in
// place of io.EOF — so a consumer copying the body into a temporary file sees
// an error before it can commit the data.
func NewVerifyingReader(r io.Reader, h hash.Hash, want []byte, mismatch error) io.Reader {
	return &verifyingReader{r: r, h: h, want: want, mismatch: mismatch}
}

type verifyingReader struct {
	r        io.Reader
	h        hash.Hash
	want     []byte
	mismatch error
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	if n > 0 {
		_, _ = v.h.Write(p[:n])
	}
	if err == io.EOF && !bytes.Equal(v.h.Sum(nil), v.want) {
		return n, v.mismatch
	}
	return n, err
}
