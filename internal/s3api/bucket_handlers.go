package s3api

import (
	"encoding/xml"
	"io"
	"net/http"

	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

// handleListBuckets handles GET / (ListBuckets).
func (rt *Router) handleListBuckets(w http.ResponseWriter, _ *http.Request) {
	buckets, err := rt.engine.ListBuckets()
	if handleStorageError(w, err, "/") {
		return
	}

	var bucketList []BucketXML
	for _, b := range buckets {
		bucketList = append(bucketList, BucketXML{
			Name:         b.Name,
			CreationDate: b.CreationDate.UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}

	result := ListAllMyBucketsResult{
		Xmlns: s3XmlNamespace,
		Owner: Owner{
			ID:          "stiva",
			DisplayName: "stiva",
		},
		Buckets: BucketsList{
			Bucket: bucketList,
		},
	}

	writeXML(w, http.StatusOK, result)
}

// handleCreateBucket handles PUT /<bucket> (CreateBucket).
func (rt *Router) handleCreateBucket(w http.ResponseWriter, _ *http.Request, bucket string) {
	if !storage.IsValidBucketName(bucket) {
		writeS3Error(w, "InvalidBucketName", "The specified bucket is not valid.", "/"+bucket)
		return
	}

	if err := rt.engine.CreateBucket(bucket); handleStorageError(w, err, "/"+bucket) {
		return
	}

	w.Header().Set("Location", "/"+bucket)
	w.WriteHeader(http.StatusOK)
}

// handleDeleteBucket handles DELETE /<bucket> (DeleteBucket).
func (rt *Router) handleDeleteBucket(w http.ResponseWriter, _ *http.Request, bucket string) {
	if err := rt.engine.DeleteBucket(bucket); handleStorageError(w, err, "/"+bucket) {
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleHeadBucket handles HEAD /<bucket> (HeadBucket).
func (rt *Router) handleHeadBucket(w http.ResponseWriter, _ *http.Request, bucket string) {
	exists, err := rt.engine.BucketExists(bucket)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}
	if !exists {
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist", "/"+bucket)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// handleGetBucketLocation handles GET /<bucket>?location (GetBucketLocation).
func (rt *Router) handleGetBucketLocation(w http.ResponseWriter, _ *http.Request, bucket string) {
	exists, err := rt.engine.BucketExists(bucket)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}
	if !exists {
		writeS3Error(w, "NoSuchBucket", "The specified bucket does not exist", "/"+bucket)
		return
	}

	result := LocationConstraintResult{
		Xmlns:    s3XmlNamespace,
		Location: rt.region,
	}
	writeXML(w, http.StatusOK, result)
}

// handleGetBucketVersioning handles GET /<bucket>?versioning.
func (rt *Router) handleGetBucketVersioning(w http.ResponseWriter, _ *http.Request, bucket string) {
	status, err := rt.engine.GetBucketVersioning(bucket)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}

	result := VersioningConfigurationXML{
		Xmlns: s3XmlNamespace,
	}
	if status == "Enabled" || status == "Suspended" {
		result.Status = status
	}

	writeXML(w, http.StatusOK, result)
}

// handlePutBucketVersioning handles PUT /<bucket>?versioning.
func (rt *Router) handlePutBucketVersioning(w http.ResponseWriter, r *http.Request, bucket string) {
	var reqBody VersioningConfigurationXML
	if err := xml.NewDecoder(io.LimitReader(r.Body, maxXMLRequestBody)).Decode(&reqBody); err != nil {
		writeS3Error(w, "MalformedXML", "The XML you provided was not well-formed", "/"+bucket)
		return
	}

	status := reqBody.Status
	// S3 accepts empty status to disable or explicit Suspended/Enabled
	if status == "" {
		status = "Disabled"
	}

	err := rt.engine.SetBucketVersioning(bucket, status)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}

	w.WriteHeader(http.StatusOK)
}

// handleGetBucketLifecycle handles GET /<bucket>?lifecycle.
func (rt *Router) handleGetBucketLifecycle(w http.ResponseWriter, _ *http.Request, bucket string) {
	lc, err := rt.engine.GetBucketLifecycle(bucket)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}

	writeXML(w, http.StatusOK, lc)
}

// handlePutBucketLifecycle handles PUT /<bucket>?lifecycle.
func (rt *Router) handlePutBucketLifecycle(w http.ResponseWriter, r *http.Request, bucket string) {
	var req storage.LifecycleConfiguration
	if err := xml.NewDecoder(io.LimitReader(r.Body, maxXMLRequestBody)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "The XML you provided was not well-formed", "/"+bucket)
		return
	}

	err := rt.engine.PutBucketLifecycle(bucket, &req)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}

	w.WriteHeader(http.StatusOK)
}

// handleDeleteBucketLifecycle handles DELETE /<bucket>?lifecycle.
func (rt *Router) handleDeleteBucketLifecycle(w http.ResponseWriter, _ *http.Request, bucket string) {
	err := rt.engine.DeleteBucketLifecycle(bucket)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGetBucketLogging handles GET /<bucket>?logging.
func (rt *Router) handleGetBucketLogging(w http.ResponseWriter, _ *http.Request, bucket string) {
	logging, err := rt.engine.GetBucketLogging(bucket)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}

	var res storage.BucketLoggingStatus
	if logging != nil {
		res = *logging
	}

	writeXML(w, http.StatusOK, res)
}

// handlePutBucketLogging handles PUT /<bucket>?logging.
func (rt *Router) handlePutBucketLogging(w http.ResponseWriter, r *http.Request, bucket string) {
	var req storage.BucketLoggingStatus
	if err := xml.NewDecoder(io.LimitReader(r.Body, maxXMLRequestBody)).Decode(&req); err != nil {
		writeS3Error(w, "MalformedXML", "The XML you provided was not well-formed", "/"+bucket)
		return
	}

	err := rt.engine.PutBucketLogging(bucket, &req)
	if handleStorageError(w, err, "/"+bucket) {
		return
	}

	w.WriteHeader(http.StatusOK)
}
