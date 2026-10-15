package storage

import (
	"context"
	"encoding/xml"
	"errors"
	"strings"
	"testing"
	"time"
)

// newLifecycleTestEngine returns an engine with one bucket holding an object
// under logs/ and one outside it, both backdated so any rule matching them
// with Days=1 would expire them.
func newLifecycleTestEngine(t *testing.T) (*FilesystemEngine, string) {
	t.Helper()
	engine, err := NewFilesystemEngine(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("NewFilesystemEngine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })

	const bucket = "lifecycle-bucket"
	if err := engine.CreateBucket(bucket); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	for _, key := range []string{"logs/old.log", "keep/important.dat"} {
		info, err := engine.PutObject(context.Background(), bucket, key, strings.NewReader("x"), 1, "text/plain")
		if err != nil {
			t.Fatalf("PutObject %s: %v", key, err)
		}
		info.LastModified = time.Now().UTC().Add(-48 * time.Hour)
		if err := engine.Metadata().PutObjectMetaRaw(info); err != nil {
			t.Fatalf("PutObjectMetaRaw %s: %v", key, err)
		}
	}
	return engine, bucket
}

func objectExists(t *testing.T, engine *FilesystemEngine, bucket, key string) bool {
	t.Helper()
	_, err := engine.HeadObject(context.Background(), bucket, key, "")
	return err == nil
}

// TestLifecyclePrefixFormsOnlyExpireMatchingKeys is the regression test for
// rules that silently applied to the whole bucket. Only <Filter><Prefix> was
// parsed; the legacy rule-level <Prefix> (still sent by s3cmd and older SDKs)
// and <Filter><And><Prefix> both decoded to an empty prefix, so a rule meant
// for logs/ expired every object in the bucket.
func TestLifecyclePrefixFormsOnlyExpireMatchingKeys(t *testing.T) {
	cases := map[string]string{
		"filter prefix": `<LifecycleConfiguration><Rule><ID>r</ID><Filter><Prefix>logs/</Prefix></Filter>` +
			`<Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`,
		"legacy rule-level prefix": `<LifecycleConfiguration><Rule><ID>r</ID><Prefix>logs/</Prefix>` +
			`<Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`,
		"and operator prefix": `<LifecycleConfiguration><Rule><ID>r</ID><Filter><And><Prefix>logs/</Prefix></And></Filter>` +
			`<Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			engine, bucket := newLifecycleTestEngine(t)

			var lc LifecycleConfiguration
			if err := xml.Unmarshal([]byte(body), &lc); err != nil {
				t.Fatalf("xml.Unmarshal: %v", err)
			}
			if err := engine.PutBucketLifecycle(bucket, &lc); err != nil {
				t.Fatalf("PutBucketLifecycle: %v", err)
			}
			if err := engine.CleanExpiredObjects(context.Background()); err != nil {
				t.Fatalf("CleanExpiredObjects: %v", err)
			}

			if objectExists(t, engine, bucket, "logs/old.log") {
				t.Error("logs/old.log should have expired")
			}
			if !objectExists(t, engine, bucket, "keep/important.dat") {
				t.Fatal("keep/important.dat was deleted by a rule scoped to logs/")
			}
		})
	}
}

// TestLifecycleUnsupportedFiltersAreRejected covers filter elements Stiva
// does not evaluate. Accepting them would mean ignoring the condition — which
// widens the rule to more objects than the caller asked for — so they must be
// refused outright.
func TestLifecycleUnsupportedFiltersAreRejected(t *testing.T) {
	cases := map[string]string{
		"tag filter": `<LifecycleConfiguration><Rule><ID>r</ID><Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter>` +
			`<Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`,
		"and with tag": `<LifecycleConfiguration><Rule><ID>r</ID><Filter><And><Prefix>logs/</Prefix><Tag><Key>k</Key><Value>v</Value></Tag></And></Filter>` +
			`<Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`,
		"object size filter": `<LifecycleConfiguration><Rule><ID>r</ID><Filter><ObjectSizeGreaterThan>1024</ObjectSizeGreaterThan></Filter>` +
			`<Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			engine, bucket := newLifecycleTestEngine(t)

			var lc LifecycleConfiguration
			if err := xml.Unmarshal([]byte(body), &lc); err != nil {
				t.Fatalf("xml.Unmarshal: %v", err)
			}
			err := engine.PutBucketLifecycle(bucket, &lc)
			var s3Err *S3Error
			if !errors.As(err, &s3Err) || s3Err.Code != "NotImplemented" {
				t.Fatalf("PutBucketLifecycle error = %v, want NotImplemented", err)
			}

			if _, err := engine.GetBucketLifecycle(bucket); err == nil {
				t.Error("a rejected configuration must not be stored")
			}
		})
	}
}

func TestLifecycleMalformedRulesAreRejected(t *testing.T) {
	cases := map[string]*LifecycleConfiguration{
		"no rules": {},
		"prefix and filter prefix both set": {Rules: []LifecycleRule{{
			ID: "r", Status: "Enabled", Prefix: "a/", Filter: LifecycleFilter{Prefix: "b/"},
			Expiration: &LifecycleExpiration{Days: 1},
		}}},
		"unknown status": {Rules: []LifecycleRule{{
			ID: "r", Status: "On", Expiration: &LifecycleExpiration{Days: 1},
		}}},
		"negative days": {Rules: []LifecycleRule{{
			ID: "r", Status: "Enabled", Expiration: &LifecycleExpiration{Days: -1},
		}}},
	}

	for name, lc := range cases {
		t.Run(name, func(t *testing.T) {
			engine, bucket := newLifecycleTestEngine(t)
			err := engine.PutBucketLifecycle(bucket, lc)
			var s3Err *S3Error
			if !errors.As(err, &s3Err) {
				t.Fatalf("PutBucketLifecycle error = %v, want an S3Error", err)
			}
		})
	}
}

// TestLifecycleRuleEffectivePrefix pins down the normalization the sweep relies on.
func TestLifecycleRuleEffectivePrefix(t *testing.T) {
	cases := []struct {
		rule LifecycleRule
		want string
	}{
		{LifecycleRule{}, ""},
		{LifecycleRule{Prefix: "legacy/"}, "legacy/"},
		{LifecycleRule{Filter: LifecycleFilter{Prefix: "filter/"}}, "filter/"},
		{LifecycleRule{Filter: LifecycleFilter{And: &LifecycleAndOperator{Prefix: "and/"}}}, "and/"},
	}
	for _, tc := range cases {
		if got := tc.rule.EffectivePrefix(); got != tc.want {
			t.Errorf("EffectivePrefix(%+v) = %q, want %q", tc.rule, got, tc.want)
		}
	}
}

// TestCleanupSweepsStopWhenCancelled: the hourly sweeps used to ignore
// shutdown entirely, so Close could run while a sweep was still deleting
// objects and using the metadata databases. They now take a context and stop
// between items once it is cancelled.
func TestCleanupSweepsStopWhenCancelled(t *testing.T) {
	engine, bucket := newLifecycleTestEngine(t)
	lc := &LifecycleConfiguration{Rules: []LifecycleRule{{
		ID: "all", Status: "Enabled", Expiration: &LifecycleExpiration{Days: 1},
	}}}
	if err := engine.PutBucketLifecycle(bucket, lc); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CreateMultipartUpload(context.Background(), bucket, "upload.bin", "application/octet-stream"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := engine.CleanExpiredObjects(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("CleanExpiredObjects with a cancelled context returned %v, want context.Canceled", err)
	}
	if !objectExists(t, engine, bucket, "logs/old.log") || !objectExists(t, engine, bucket, "keep/important.dat") {
		t.Error("a cancelled sweep still deleted objects")
	}

	if err := engine.CleanExpiredMultipartUploads(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("CleanExpiredMultipartUploads with a cancelled context returned %v, want context.Canceled", err)
	}
	uploads, _, err := engine.ListMultipartUploads(bucket, "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(uploads) != 1 {
		t.Error("a cancelled sweep still aborted multipart uploads")
	}
}
