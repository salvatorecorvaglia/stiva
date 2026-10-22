package console

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"

	"github.com/salvatorecorvaglia/stiva/internal/httpx"
	"github.com/salvatorecorvaglia/stiva/internal/storage"
)

func (h *Handler) getVersioning(w http.ResponseWriter, _ *http.Request, bucket string) {
	status, err := h.engine.GetBucketVersioning(bucket)
	if err != nil {
		writeStorageError(w, err, "failed to read versioning")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (h *Handler) setVersioning(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxJSONRequestBody)).Decode(&req); err != nil || (req.Status != "Enabled" && req.Status != "Suspended") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be Enabled or Suspended"})
		return
	}
	if err := h.engine.SetBucketVersioning(bucket, req.Status); err != nil {
		writeStorageError(w, err, "failed to update versioning")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": req.Status})
}

func (h *Handler) listVersions(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	result, err := h.engine.ListObjectVersions(&storage.ListVersionsInput{
		Bucket: bucket, Prefix: q.Get("prefix"),
		MaxKeys:   httpx.MaxKeys(q.Get("maxKeys"), httpx.MaxPageSize),
		KeyMarker: q.Get("keyMarker"), VersionIDMarker: q.Get("versionIdMarker"),
	})
	if err != nil {
		writeStorageError(w, err, "failed to list versions")
		return
	}
	// Only expose browser metadata, never internal blob IDs or encryption metadata.
	type version struct {
		Key            string `json:"key"`
		VersionID      string `json:"versionId"`
		Size           int64  `json:"size"`
		LastModified   string `json:"lastModified"`
		IsLatest       bool   `json:"isLatest"`
		IsDeleteMarker bool   `json:"isDeleteMarker"`
	}
	items := []version{}
	for _, group := range [][]storage.ObjectInfo{result.Versions, result.DeleteMarkers} {
		for _, obj := range group {
			items = append(items, version{obj.Key, obj.VersionID, obj.Size, obj.LastModified.UTC().Format("2006-01-02T15:04:05.000Z"), obj.IsLatest, obj.IsDeleteMarker})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Key != items[j].Key {
			return items[i].Key < items[j].Key
		}
		return items[i].LastModified > items[j].LastModified
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items": items, "isTruncated": result.IsTruncated,
		"nextKeyMarker": result.NextKeyMarker, "nextVersionIdMarker": result.NextVersionIDMarker,
	})
}
