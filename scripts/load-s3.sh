#!/usr/bin/env bash
# Bounded AWS CLI load exercise. Run against a staging endpoint only.
set -euo pipefail
: "${STIVA_TEST_ENDPOINT:?Required}"
: "${AWS_ACCESS_KEY_ID:?Required}"
: "${AWS_SECRET_ACCESS_KEY:?Required}"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
export AWS_PAGER="" AWS_EC2_METADATA_DISABLED=true
workers=${STIVA_LOAD_WORKERS:-8}
objects=${STIVA_LOAD_OBJECTS:-100}
[[ "$workers" =~ ^[1-9][0-9]*$ && "$objects" =~ ^[1-9][0-9]*$ ]]
(( workers <= 128 && objects <= 10000 ))
bucket="stiva-load-$(date +%s)-${RANDOM}"
work=$(mktemp -d)
cleanup(){ aws --endpoint-url "$STIVA_TEST_ENDPOINT" s3 rb "s3://$bucket" --force >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT
dd if=/dev/urandom of="$work/payload" bs=1048576 count=1 2>/dev/null
aws --endpoint-url "$STIVA_TEST_ENDPOINT" s3api create-bucket --bucket "$bucket" >/dev/null
export STIVA_LOAD_BUCKET="$bucket" STIVA_LOAD_WORKDIR="$work"
start=$(date +%s)
seq 1 "$objects" | xargs -P "$workers" -I '{}' bash -c '
 set -euo pipefail
 aws --endpoint-url "$STIVA_TEST_ENDPOINT" s3api put-object --bucket "$STIVA_LOAD_BUCKET" --key "object-$1" --body "$STIVA_LOAD_WORKDIR/payload" >/dev/null
 aws --endpoint-url "$STIVA_TEST_ENDPOINT" s3api get-object --bucket "$STIVA_LOAD_BUCKET" --key "object-$1" "$STIVA_LOAD_WORKDIR/download-$1" >/dev/null
 cmp "$STIVA_LOAD_WORKDIR/payload" "$STIVA_LOAD_WORKDIR/download-$1"
 rm "$STIVA_LOAD_WORKDIR/download-$1"
' _ '{}'
printf 'Verified %s objects of 1 MiB with %s workers in %s seconds\n' "$objects" "$workers" "$(( $(date +%s) - start ))"
