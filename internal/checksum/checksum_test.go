package checksum

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"testing"
)

// The standard CRC check values, over the ASCII bytes "123456789".
func TestAlgorithmsMatchStandardCheckValues(t *testing.T) {
	cases := []struct {
		header string
		want   []byte
	}{
		{"x-amz-checksum-crc32", be32(0xCBF43926)},
		{"x-amz-checksum-crc32c", be32(0xE3069283)},
		{"x-amz-checksum-crc64nvme", be64(0xAE8B14860A799888)},
	}
	for _, tc := range cases {
		h, ok := New(tc.header)
		if !ok {
			t.Fatalf("New(%q) not supported", tc.header)
		}
		h.Write([]byte("123456789"))
		if got := h.Sum(nil); !bytes.Equal(got, tc.want) {
			t.Errorf("%s = %x, want %x", tc.header, got, tc.want)
		}
	}
	for _, name := range []string{"x-amz-checksum-sha1", "x-amz-checksum-sha256"} {
		if _, ok := New(name); !ok {
			t.Errorf("New(%q) not supported", name)
		}
	}
	if _, ok := New("x-amz-checksum-md4"); ok {
		t.Error("an unknown algorithm was accepted")
	}
}

func TestVerifyingReader(t *testing.T) {
	data := []byte("payload")
	h, _ := New("x-amz-checksum-crc32")
	h.Write(data)
	good := h.Sum(nil)
	mismatch := errors.New("mismatch")

	fresh := func() hash.Hash {
		h, _ := New("x-amz-checksum-crc32")
		return h
	}

	got, err := io.ReadAll(NewVerifyingReader(bytes.NewReader(data), fresh(), good, mismatch))
	if err != nil || string(got) != "payload" {
		t.Fatalf("matching checksum: got %q, %v", got, err)
	}

	bad := append([]byte(nil), good...)
	bad[0] ^= 0xff
	_, err = io.ReadAll(NewVerifyingReader(bytes.NewReader(data), fresh(), bad, mismatch))
	if !errors.Is(err, mismatch) {
		t.Fatalf("mismatched checksum: err = %v, want the mismatch error at EOF", err)
	}
}

func TestDecodeExpected(t *testing.T) {
	h, _ := New("x-amz-checksum-crc32")
	if _, err := DecodeExpected(base64.StdEncoding.EncodeToString(be32(1)), h); err != nil {
		t.Errorf("valid value rejected: %v", err)
	}
	for _, raw := range []string{"not base64!", base64.StdEncoding.EncodeToString([]byte{1, 2})} {
		if _, err := DecodeExpected(raw, h); err == nil {
			t.Errorf("DecodeExpected(%q) accepted a malformed value", raw)
		}
	}
}

func be32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func be64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }
