package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"

	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// maxDeleteObjectsPerRequest matches the S3 limit on batch delete size.
const maxDeleteObjectsPerRequest = 1000

// maxXMLRequestBody bounds the XML body accepted by subresource/config PUT
// endpoints and batch delete, so a client that skips real payload signing
// (UNSIGNED-PAYLOAD, or any body-size mode not covered by auth.HashPayload's
// checks) cannot force unbounded xml.Decoder allocation.
const maxXMLRequestBody = 2 << 20 // 2MB

// handleDeleteObjects handles POST /<bucket>?delete (DeleteObjects).
//
// This is the batch-delete call the AWS CLI issues for `s3 rm --recursive` and
// `s3 sync --delete`. Without it those commands fail outright, so recursive
// deletion against Stiva was not possible through standard tooling.
func (rt *Router) handleDeleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	resource := "/" + bucket

	var req DeleteObjectsRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, maxXMLRequestBody)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "The XML you provided was not well-formed", resource)
		return
	}

	if len(req.Objects) == 0 {
		writeS3Error(w, "MalformedXML", "The request must contain at least one Object", resource)
		return
	}
	if len(req.Objects) > maxDeleteObjectsPerRequest {
		writeS3Error(w, "MalformedXML",
			"The request contains more than the maximum number of keys allowed in a single delete", resource)
		return
	}

	result := DeleteObjectsResult{Xmlns: s3XmlNamespace}

	for _, obj := range req.Objects {
		if obj.Key == "" {
			result.Errors = append(result.Errors, DeleteObjectError{
				Key:     obj.Key,
				Code:    "InvalidArgument",
				Message: "The object key must not be empty",
			})
			continue
		}

		isMarker, markerVersionID, err := rt.engine.DeleteObject(bucket, obj.Key, obj.VersionId)
		if err != nil {
			code, message := "InternalError", "We encountered an internal error. Please try again."
			var s3Err *storage.S3Error
			if errors.As(err, &s3Err) {
				code, message = s3Err.Code, s3Err.Message
			}
			result.Errors = append(result.Errors, DeleteObjectError{
				Key:       obj.Key,
				VersionId: obj.VersionId,
				Code:      code,
				Message:   message,
			})
			continue
		}

		// Quiet mode suppresses the per-key success entries, as in S3.
		if req.Quiet {
			continue
		}
		result.Deleted = append(result.Deleted, DeletedObjectXML{
			Key:                   obj.Key,
			VersionId:             obj.VersionId,
			DeleteMarker:          isMarker,
			DeleteMarkerVersionId: markerVersionID,
		})
	}

	writeXML(w, http.StatusOK, result)
}
