package storage

// GetBucketVersioning gets the versioning status of a bucket.
func (fs *FilesystemEngine) GetBucketVersioning(bucket string) (string, error) {
	if err := fs.requireBucket(bucket); err != nil {
		return "", err
	}
	return fs.metadata.GetBucketVersioning(bucket)
}

// SetBucketVersioning sets the versioning status of a bucket.
func (fs *FilesystemEngine) SetBucketVersioning(bucket, status string) error {
	unlock := fs.lockBucket(bucket, true)
	defer unlock()
	if err := fs.requireBucket(bucket); err != nil {
		return err
	}
	if status != "Enabled" && status != "Suspended" && status != "Disabled" {
		return &S3Error{Code: "InvalidArgument", Message: "Versioning status must be Enabled, Suspended, or Disabled"}
	}
	if status == "Disabled" {
		// As in S3, a versioned bucket can be suspended but never returned to
		// unversioned. Allowing it sent the next plain DELETE down the
		// unversioned path, which erases every version of the key.
		current, err := fs.metadata.GetBucketVersioning(bucket)
		if err != nil {
			return err
		}
		if current == "Enabled" || current == "Suspended" {
			return &S3Error{
				Code:    "IllegalVersioningConfigurationException",
				Message: "A versioned bucket cannot be returned to an unversioned state; use Suspended instead.",
			}
		}
		// Never versioned: Disabled is a no-op.
		return nil
	}
	return fs.metadata.PutBucketVersioning(bucket, status)
}
