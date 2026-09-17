// Package s3api implements the S3-compatible REST API handlers.
package s3api

import "encoding/xml"

// --- ListBuckets ---

// ListAllMyBucketsResult is the XML response for ListBuckets.
type ListAllMyBucketsResult struct {
	XMLName xml.Name    `xml:"ListAllMyBucketsResult"`
	Xmlns   string      `xml:"xmlns,attr"`
	Owner   Owner       `xml:"Owner"`
	Buckets BucketsList `xml:"Buckets"`
}

// Owner represents the bucket owner.
type Owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

// BucketsList wraps a list of buckets.
type BucketsList struct {
	Bucket []BucketXML `xml:"Bucket"`
}

// BucketXML represents a single bucket in XML responses.
type BucketXML struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

// --- ListObjectsV2 ---

// ListBucketResult is the XML response for ListObjectsV2.
type ListBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Xmlns                 string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	MaxKeys               int            `xml:"MaxKeys"`
	IsTruncated           bool           `xml:"IsTruncated"`
	KeyCount              int            `xml:"KeyCount"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	Contents              []ContentsXML  `xml:"Contents"`
	CommonPrefixes        []CommonPrefix `xml:"CommonPrefixes,omitempty"`
}

// ContentsXML represents a single object in a list response.
type ContentsXML struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

// CommonPrefix represents a common prefix (virtual directory) in list responses.
type CommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// --- ListObjects (V1) ---

// ListBucketResultV1 is the XML response for the original ListObjects call.
// V1 clients page with Marker/NextMarker rather than continuation tokens.
type ListBucketResultV1 struct {
	XMLName        xml.Name       `xml:"ListBucketResult"`
	Xmlns          string         `xml:"xmlns,attr"`
	Name           string         `xml:"Name"`
	Prefix         string         `xml:"Prefix"`
	Marker         string         `xml:"Marker"`
	NextMarker     string         `xml:"NextMarker,omitempty"`
	Delimiter      string         `xml:"Delimiter,omitempty"`
	MaxKeys        int            `xml:"MaxKeys"`
	IsTruncated    bool           `xml:"IsTruncated"`
	Contents       []ContentsXML  `xml:"Contents"`
	CommonPrefixes []CommonPrefix `xml:"CommonPrefixes,omitempty"`
}

// --- ListObjectVersions ---

// ListVersionsResult is the XML response for GET /<bucket>?versions.
type ListVersionsResult struct {
	XMLName             xml.Name           `xml:"ListVersionsResult"`
	Xmlns               string             `xml:"xmlns,attr"`
	Name                string             `xml:"Name"`
	Prefix              string             `xml:"Prefix"`
	KeyMarker           string             `xml:"KeyMarker"`
	VersionIdMarker     string             `xml:"VersionIdMarker"`
	NextKeyMarker       string             `xml:"NextKeyMarker,omitempty"`
	NextVersionIdMarker string             `xml:"NextVersionIdMarker,omitempty"`
	Delimiter           string             `xml:"Delimiter,omitempty"`
	MaxKeys             int                `xml:"MaxKeys"`
	IsTruncated         bool               `xml:"IsTruncated"`
	Versions            []ObjectVersionXML `xml:"Version"`
	DeleteMarkers       []DeleteMarkerXML  `xml:"DeleteMarker"`
	CommonPrefixes      []CommonPrefix     `xml:"CommonPrefixes,omitempty"`
}

// ObjectVersionXML is a single non-delete-marker version entry.
type ObjectVersionXML struct {
	Key          string `xml:"Key"`
	VersionId    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        Owner  `xml:"Owner"`
}

// DeleteMarkerXML is a single delete-marker entry.
type DeleteMarkerXML struct {
	Key          string `xml:"Key"`
	VersionId    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	Owner        Owner  `xml:"Owner"`
}

// --- DeleteObjects (batch delete) ---

// DeleteObjectsRequest is the XML body for POST /<bucket>?delete.
type DeleteObjectsRequest struct {
	XMLName xml.Name            `xml:"Delete"`
	Quiet   bool                `xml:"Quiet"`
	Objects []DeleteObjectEntry `xml:"Object"`
}

// DeleteObjectEntry is a single key (optionally version) to delete.
type DeleteObjectEntry struct {
	Key       string `xml:"Key"`
	VersionId string `xml:"VersionId,omitempty"`
}

// DeleteObjectsResult is the XML response for batch delete.
type DeleteObjectsResult struct {
	XMLName xml.Name            `xml:"DeleteResult"`
	Xmlns   string              `xml:"xmlns,attr"`
	Deleted []DeletedObjectXML  `xml:"Deleted"`
	Errors  []DeleteObjectError `xml:"Error"`
}

// DeletedObjectXML reports one successfully deleted key.
type DeletedObjectXML struct {
	Key                   string `xml:"Key"`
	VersionId             string `xml:"VersionId,omitempty"`
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionId string `xml:"DeleteMarkerVersionId,omitempty"`
}

// DeleteObjectError reports one key that could not be deleted.
type DeleteObjectError struct {
	Key       string `xml:"Key"`
	VersionId string `xml:"VersionId,omitempty"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
}

// --- ListMultipartUploads / ListParts ---

// ListMultipartUploadsResult is the XML response for GET /<bucket>?uploads.
type ListMultipartUploadsResult struct {
	XMLName            xml.Name          `xml:"ListMultipartUploadsResult"`
	Xmlns              string            `xml:"xmlns,attr"`
	Bucket             string            `xml:"Bucket"`
	KeyMarker          string            `xml:"KeyMarker"`
	UploadIdMarker     string            `xml:"UploadIdMarker"`
	NextKeyMarker      string            `xml:"NextKeyMarker,omitempty"`
	NextUploadIdMarker string            `xml:"NextUploadIdMarker,omitempty"`
	Prefix             string            `xml:"Prefix,omitempty"`
	Delimiter          string            `xml:"Delimiter,omitempty"`
	MaxUploads         int               `xml:"MaxUploads"`
	IsTruncated        bool              `xml:"IsTruncated"`
	Uploads            []MultipartUpload `xml:"Upload"`
	CommonPrefixes     []CommonPrefix    `xml:"CommonPrefixes,omitempty"`
}

// MultipartUpload is a single in-progress upload entry.
type MultipartUpload struct {
	Key          string `xml:"Key"`
	UploadId     string `xml:"UploadId"`
	Initiated    string `xml:"Initiated"`
	StorageClass string `xml:"StorageClass"`
	Owner        Owner  `xml:"Owner"`
	Initiator    Owner  `xml:"Initiator"`
}

// ListPartsResult is the XML response for GET /<bucket>/<key>?uploadId=...
type ListPartsResult struct {
	XMLName              xml.Name      `xml:"ListPartsResult"`
	Xmlns                string        `xml:"xmlns,attr"`
	Bucket               string        `xml:"Bucket"`
	Key                  string        `xml:"Key"`
	UploadId             string        `xml:"UploadId"`
	PartNumberMarker     int           `xml:"PartNumberMarker"`
	NextPartNumberMarker int           `xml:"NextPartNumberMarker,omitempty"`
	MaxParts             int           `xml:"MaxParts"`
	IsTruncated          bool          `xml:"IsTruncated"`
	StorageClass         string        `xml:"StorageClass"`
	Owner                Owner         `xml:"Owner"`
	Initiator            Owner         `xml:"Initiator"`
	Parts                []ListPartXML `xml:"Part"`
}

// ListPartXML is a single uploaded part entry.
type ListPartXML struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

// --- CreateBucket ---

// CreateBucketConfiguration is the XML body for CreateBucket requests.
type CreateBucketConfiguration struct {
	XMLName            xml.Name `xml:"CreateBucketConfiguration"`
	LocationConstraint string   `xml:"LocationConstraint"`
}

// --- Multipart Upload ---

// InitiateMultipartUploadResult is the XML response for CreateMultipartUpload.
type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadId string   `xml:"UploadId"`
}

// CompleteMultipartUploadRequest is the XML body for CompleteMultipartUpload.
type CompleteMultipartUploadRequest struct {
	XMLName xml.Name  `xml:"CompleteMultipartUpload"`
	Parts   []PartXML `xml:"Part"`
}

// PartXML represents a part in multipart upload XML.
type PartXML struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// CompleteMultipartUploadResult is the XML response for CompleteMultipartUpload.
type CompleteMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// --- CopyObject ---

// CopyObjectResult is the XML response for CopyObject.
type CopyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

// --- UploadPartCopy ---

// CopyPartResult is the XML response for UploadPartCopy.
type CopyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

// --- Error ---

// ErrorResponse is the XML error response format.
type ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestId string   `xml:"RequestId"`
}

// --- Location ---

// LocationConstraint is the XML response for GetBucketLocation.
type LocationConstraintResult struct {
	XMLName  xml.Name `xml:"LocationConstraint"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:",chardata"`
}

// --- CORS ---

// CORSConfigurationXML represents S3 CORS XML body.
type CORSConfigurationXML struct {
	XMLName   xml.Name      `xml:"CORSConfiguration"`
	Xmlns     string        `xml:"xmlns,attr,omitempty"`
	CORSRules []CORSRuleXML `xml:"CORSRule"`
}

// CORSRuleXML represents a single S3 CORS XML rule.
type CORSRuleXML struct {
	AllowedHeader []string `xml:"AllowedHeader,omitempty"`
	AllowedMethod []string `xml:"AllowedMethod"`
	AllowedOrigin []string `xml:"AllowedOrigin"`
	ExposeHeader  []string `xml:"ExposeHeader,omitempty"`
	MaxAgeSeconds int      `xml:"MaxAgeSeconds,omitempty"`
}

// --- Versioning ---

// VersioningConfigurationXML represents S3 bucket versioning configuration XML body/response.
type VersioningConfigurationXML struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Xmlns   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status,omitempty"`
}
