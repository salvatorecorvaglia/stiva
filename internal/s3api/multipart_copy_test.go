package s3api

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// TestUploadPartCopyRoutesToPartCopyNotWholeObjectCopy guards a routing bug
// where a genuine UploadPartCopy request (x-amz-copy-source header AND
// partNumber+uploadId query params both present) was matched by the
// copy-source case first and dispatched to whole-object CopyObject instead —
// clobbering the destination key immediately and never registering the part,
// so CompleteMultipartUpload would later fail with InvalidPart.
func TestUploadPartCopyRoutesToPartCopyNotWholeObjectCopy(t *testing.T) {
	rt, eng := newOpsRouter(t)
	putObjects(t, eng, "src-bucket", "source.txt")

	// Give the destination bucket some pre-existing content at the target key
	// so a wrongly-routed whole-object copy would visibly overwrite it before
	// the multipart upload is ever completed.
	if err := eng.CreateBucket("dst-bucket"); err != nil {
		t.Fatalf("create dst bucket: %v", err)
	}

	info, err := eng.CreateMultipartUpload("dst-bucket", "dest.txt", "application/octet-stream")
	if err != nil {
		t.Fatalf("create multipart upload: %v", err)
	}

	path := fmt.Sprintf("/dest.txt?partNumber=1&uploadId=%s", info.UploadID)
	req := httptest.NewRequest(http.MethodPut, path, nil)
	req.Header.Set("x-amz-copy-source", "/src-bucket/source.txt")
	w := httptest.NewRecorder()
	rt.handleObjectOps(w, req, "dst-bucket", "dest.txt")

	if w.Code != http.StatusOK {
		t.Fatalf("UploadPartCopy status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	var result CopyPartResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode CopyPartResult: %v (body: %s)", err, w.Body.String())
	}
	if result.ETag == "" {
		t.Errorf("expected a non-empty part ETag in the CopyPartResult")
	}

	// The destination key must not have been created directly by a
	// whole-object copy — only the multipart part should exist so far.
	if _, err := eng.HeadObject(context.Background(), "dst-bucket", "dest.txt", ""); err == nil {
		t.Fatal("dest.txt should not exist yet: UploadPartCopy must not fall through to whole-object CopyObject")
	}

	// Completing the multipart upload should now succeed using the copied part.
	etag := strings.Trim(result.ETag, `"`)
	completed, err := eng.CompleteMultipartUpload(context.Background(), "dst-bucket", "dest.txt", info.UploadID,
		[]storage.CompletePart{{PartNumber: 1, ETag: etag}})
	if err != nil {
		t.Fatalf("complete multipart upload: %v", err)
	}
	if completed.Size != 1 {
		t.Errorf("completed object size = %d, want 1 (source.txt is 1 byte)", completed.Size)
	}

	reader, _, err := eng.GetObject(context.Background(), "dst-bucket", "dest.txt", "")
	if err != nil {
		t.Fatalf("get completed object: %v", err)
	}
	defer reader.Close()
	got, _ := io.ReadAll(reader)
	if string(got) != "x" {
		t.Errorf("completed object content = %q, want %q", got, "x")
	}
}
