package s3api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/salvatorecorvaglia/stiva/internal/httpx"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

const iso8601Millis = "2006-01-02T15:04:05.000Z"

// maxKeysFromQuery reads a bounded max-keys style parameter. The bound itself
// lives in httpx so the console applies the same one.
func maxKeysFromQuery(raw string, def int) int {
	return httpx.MaxKeys(raw, def)
}

// handleListObjects dispatches GET /<bucket> to the V1 or V2 listing shape.
// Clients still using the original ListObjects call send list-type=1 (or omit
// it and page with `marker`), which previously fell through to the V2 handler
// and silently ignored their marker.
func (rt *Router) handleListObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	if r.URL.Query().Get("list-type") == "2" {
		rt.handleListObjectsV2(w, r, bucket)
		return
	}
	rt.handleListObjectsV1(w, r, bucket)
}

func (rt *Router) toContentsXML(objects []storage.ObjectInfo) []ContentsXML {
	contents := make([]ContentsXML, 0, len(objects))
	for _, obj := range objects {
		contents = append(contents, ContentsXML{
			Key:          obj.Key,
			LastModified: obj.LastModified.UTC().Format(iso8601Millis),
			ETag:         fmt.Sprintf(`"%s"`, obj.ETag),
			Size:         obj.Size,
			StorageClass: "STANDARD",
		})
	}
	return contents
}

func toCommonPrefixes(prefixes []string) []CommonPrefix {
	out := make([]CommonPrefix, 0, len(prefixes))
	for _, p := range prefixes {
		out = append(out, CommonPrefix{Prefix: p})
	}
	return out
}

// handleListObjectsV2 handles GET /<bucket>?list-type=2 (ListObjectsV2).
func (rt *Router) handleListObjectsV2(w http.ResponseWriter, r *http.Request, bucket string) {
	query := r.URL.Query()
	maxKeys := maxKeysFromQuery(query.Get("max-keys"), 1000)

	input := &storage.ListObjectsInput{
		Bucket:            bucket,
		Prefix:            query.Get("prefix"),
		Delimiter:         query.Get("delimiter"),
		MaxKeys:           maxKeys,
		ContinuationToken: query.Get("continuation-token"),
		StartAfter:        query.Get("start-after"),
	}

	output, err := rt.engine.ListObjects(input)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}

	writeXML(w, http.StatusOK, ListBucketResult{
		Xmlns:                 s3XmlNamespace,
		Name:                  bucket,
		Prefix:                input.Prefix,
		Delimiter:             input.Delimiter,
		MaxKeys:               maxKeys,
		IsTruncated:           output.IsTruncated,
		KeyCount:              output.KeyCount,
		ContinuationToken:     input.ContinuationToken,
		NextContinuationToken: output.NextContinuationToken,
		StartAfter:            input.StartAfter,
		Contents:              rt.toContentsXML(output.Objects),
		CommonPrefixes:        toCommonPrefixes(output.CommonPrefixes),
	})
}

// handleListObjectsV1 handles GET /<bucket> (the original ListObjects), which
// pages with Marker/NextMarker instead of continuation tokens.
func (rt *Router) handleListObjectsV1(w http.ResponseWriter, r *http.Request, bucket string) {
	query := r.URL.Query()
	maxKeys := maxKeysFromQuery(query.Get("max-keys"), 1000)
	marker := query.Get("marker")

	input := &storage.ListObjectsInput{
		Bucket:     bucket,
		Prefix:     query.Get("prefix"),
		Delimiter:  query.Get("delimiter"),
		MaxKeys:    maxKeys,
		StartAfter: marker,
	}

	output, err := rt.engine.ListObjects(input)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}

	// NextMarker is only meaningful when the response is truncated.
	nextMarker := ""
	if output.IsTruncated {
		nextMarker = output.NextContinuationToken
	}

	writeXML(w, http.StatusOK, ListBucketResultV1{
		Xmlns:          s3XmlNamespace,
		Name:           bucket,
		Prefix:         input.Prefix,
		Marker:         marker,
		NextMarker:     nextMarker,
		Delimiter:      input.Delimiter,
		MaxKeys:        maxKeys,
		IsTruncated:    output.IsTruncated,
		Contents:       rt.toContentsXML(output.Objects),
		CommonPrefixes: toCommonPrefixes(output.CommonPrefixes),
	})
}

// handleListObjectVersions handles GET /<bucket>?versions.
func (rt *Router) handleListObjectVersions(w http.ResponseWriter, r *http.Request, bucket string) {
	query := r.URL.Query()
	maxKeys := maxKeysFromQuery(query.Get("max-keys"), 1000)

	input := &storage.ListVersionsInput{
		Bucket:          bucket,
		Prefix:          query.Get("prefix"),
		Delimiter:       query.Get("delimiter"),
		MaxKeys:         maxKeys,
		KeyMarker:       query.Get("key-marker"),
		VersionIDMarker: query.Get("version-id-marker"),
	}

	output, err := rt.engine.ListObjectVersions(input)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}

	owner := Owner{ID: "stiva", DisplayName: "stiva"}

	versions := make([]ObjectVersionXML, 0, len(output.Versions))
	for _, v := range output.Versions {
		versions = append(versions, ObjectVersionXML{
			Key:          v.Key,
			VersionId:    versionIDOrNull(v.VersionID),
			IsLatest:     v.IsLatest,
			LastModified: v.LastModified.UTC().Format(iso8601Millis),
			ETag:         fmt.Sprintf(`"%s"`, v.ETag),
			Size:         v.Size,
			StorageClass: "STANDARD",
			Owner:        owner,
		})
	}

	markers := make([]DeleteMarkerXML, 0, len(output.DeleteMarkers))
	for _, d := range output.DeleteMarkers {
		markers = append(markers, DeleteMarkerXML{
			Key:          d.Key,
			VersionId:    versionIDOrNull(d.VersionID),
			IsLatest:     d.IsLatest,
			LastModified: d.LastModified.UTC().Format(iso8601Millis),
			Owner:        owner,
		})
	}

	writeXML(w, http.StatusOK, ListVersionsResult{
		Xmlns:               s3XmlNamespace,
		Name:                bucket,
		Prefix:              input.Prefix,
		KeyMarker:           input.KeyMarker,
		VersionIdMarker:     input.VersionIDMarker,
		NextKeyMarker:       output.NextKeyMarker,
		NextVersionIdMarker: output.NextVersionIDMarker,
		Delimiter:           input.Delimiter,
		MaxKeys:             maxKeys,
		IsTruncated:         output.IsTruncated,
		Versions:            versions,
		DeleteMarkers:       markers,
		CommonPrefixes:      toCommonPrefixes(output.CommonPrefixes),
	})
}

// versionIDOrNull reports the S3 sentinel "null" for unversioned objects.
func versionIDOrNull(id string) string {
	if id == "" {
		return "null"
	}
	return id
}

// handleListMultipartUploads handles GET /<bucket>?uploads.
func (rt *Router) handleListMultipartUploads(w http.ResponseWriter, r *http.Request, bucket string) {
	query := r.URL.Query()
	maxUploads := maxKeysFromQuery(query.Get("max-uploads"), 1000)
	prefix := query.Get("prefix")
	keyMarker := query.Get("key-marker")
	uploadIDMarker := query.Get("upload-id-marker")

	uploads, truncated, err := rt.engine.ListMultipartUploads(bucket, prefix, keyMarker, uploadIDMarker, maxUploads)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}

	owner := Owner{ID: "stiva", DisplayName: "stiva"}
	entries := make([]MultipartUpload, 0, len(uploads))
	for _, u := range uploads {
		entries = append(entries, MultipartUpload{
			Key:          u.Key,
			UploadId:     u.UploadID,
			Initiated:    u.Created.UTC().Format(iso8601Millis),
			StorageClass: "STANDARD",
			Owner:        owner,
			Initiator:    owner,
		})
	}

	result := ListMultipartUploadsResult{
		Xmlns:          s3XmlNamespace,
		Bucket:         bucket,
		KeyMarker:      keyMarker,
		UploadIdMarker: uploadIDMarker,
		Prefix:         prefix,
		Delimiter:      query.Get("delimiter"),
		MaxUploads:     maxUploads,
		IsTruncated:    truncated,
		Uploads:        entries,
	}
	if truncated && len(entries) > 0 {
		last := entries[len(entries)-1]
		result.NextKeyMarker = last.Key
		result.NextUploadIdMarker = last.UploadId
	}

	writeXML(w, http.StatusOK, result)
}

// handleListParts handles GET /<bucket>/<key>?uploadId=... (ListParts).
func (rt *Router) handleListParts(w http.ResponseWriter, r *http.Request, bucket, key string) {
	query := r.URL.Query()
	uploadID := query.Get("uploadId")
	resource := "/" + bucket + "/" + key

	maxParts := maxKeysFromQuery(query.Get("max-parts"), 1000)
	partNumberMarker := 0
	if v, err := strconv.Atoi(query.Get("part-number-marker")); err == nil && v > 0 {
		partNumberMarker = v
	}

	parts, err := rt.engine.ListParts(bucket, key, uploadID)
	if handleStorageError(w, err, resource) {
		return
	}

	owner := Owner{ID: "stiva", DisplayName: "stiva"}
	entries := make([]ListPartXML, 0, len(parts))
	for _, p := range parts {
		if p.PartNumber <= partNumberMarker {
			continue
		}
		if len(entries) >= maxParts {
			break
		}
		entries = append(entries, ListPartXML{
			PartNumber: p.PartNumber,
			ETag:       fmt.Sprintf(`"%s"`, p.ETag),
			Size:       p.Size,
		})
	}

	truncated := false
	next := 0
	if len(entries) > 0 {
		last := entries[len(entries)-1].PartNumber
		for _, p := range parts {
			if p.PartNumber > last {
				truncated = true
				next = last
				break
			}
		}
	}

	writeXML(w, http.StatusOK, ListPartsResult{
		Xmlns:                s3XmlNamespace,
		Bucket:               bucket,
		Key:                  key,
		UploadId:             uploadID,
		PartNumberMarker:     partNumberMarker,
		NextPartNumberMarker: next,
		MaxParts:             maxParts,
		IsTruncated:          truncated,
		StorageClass:         "STANDARD",
		Owner:                owner,
		Initiator:            owner,
		Parts:                entries,
	})
}
