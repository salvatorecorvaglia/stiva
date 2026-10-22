#!/usr/bin/env bash
# AWS CLI exercises the deployed API. Credentials are read from the environment.
set -euo pipefail
: "${STIVA_TEST_ENDPOINT:?Set STIVA_TEST_ENDPOINT}"
: "${AWS_ACCESS_KEY_ID:?Required}"
: "${AWS_SECRET_ACCESS_KEY:?Required}"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
export AWS_PAGER=""
export AWS_EC2_METADATA_DISABLED=true
bucket="stiva-smoke-$(date +%s)-${RANDOM}"
work=$(mktemp -d)
export AWS_CONFIG_FILE="$work/awsconfig"
printf '[default]\nregion = %s\ns3 =\n    signature_version = s3v4\n    addressing_style = path\n' "$AWS_DEFAULT_REGION" > "$AWS_CONFIG_FILE"
aws_s3() { aws --endpoint-url "$STIVA_TEST_ENDPOINT" "$@"; }
cleanup() {
 aws_s3 s3 rb "s3://$bucket" --force >/dev/null 2>&1 || true
 rm -rf "$work"
}
trap cleanup EXIT
printf 'stiva production smoke test\n' > "$work/source"
aws_s3 s3api create-bucket --bucket "$bucket" >/dev/null
aws_s3 s3api put-object --bucket "$bucket" --key 'folder/a b+?.txt' --body "$work/source" >/dev/null
aws_s3 s3api get-object --bucket "$bucket" --key 'folder/a b+?.txt' "$work/download" >/dev/null
cmp "$work/source" "$work/download"
aws_s3 s3api get-object --bucket "$bucket" --key 'folder/a b+?.txt' --range bytes=0-4 "$work/range" >/dev/null
printf stiva > "$work/expected-range"
cmp "$work/expected-range" "$work/range"
aws_s3 s3api copy-object --bucket "$bucket" --key copied --copy-source "$bucket/folder/a b+?.txt" >/dev/null
url=$(aws_s3 s3 presign "s3://$bucket/copied" --expires-in 60)
curl --fail --silent --show-error "$url" -o "$work/presigned"
cmp "$work/source" "$work/presigned"
# Explicit multipart, independent of a client's size threshold.
dd if=/dev/zero of="$work/part" bs=1048576 count=6 2>/dev/null
upload=$(aws_s3 s3api create-multipart-upload --bucket "$bucket" --key multipart --query UploadId --output text)
etag=$(aws_s3 s3api upload-part --bucket "$bucket" --key multipart --upload-id "$upload" --part-number 1 --body "$work/part" --query ETag --output text)
printf '{"Parts":[{"ETag":%s,"PartNumber":1}]}' "$etag" > "$work/complete.json"
aws_s3 s3api complete-multipart-upload --bucket "$bucket" --key multipart --upload-id "$upload" --multipart-upload "file://$work/complete.json" >/dev/null
aws_s3 s3api get-object --bucket "$bucket" --key multipart "$work/multipart" >/dev/null
cmp "$work/part" "$work/multipart"
aws_s3 s3api list-objects-v2 --bucket "$bucket" >/dev/null
printf 'S3 smoke test passed\n'
