#!/usr/bin/env bash
# Isolated ENOSPC test: never fills the host or production volume.
set -euo pipefail
: "${STIVA_TEST_IMAGE:?Required}"
name="stiva-full-${RANDOM}"
work=$(mktemp -d)
export AWS_ACCESS_KEY_ID=disk-full-access
AWS_SECRET_ACCESS_KEY=$(openssl rand -hex 32)
export AWS_SECRET_ACCESS_KEY AWS_EC2_METADATA_DISABLED=true AWS_DEFAULT_REGION=us-east-1
cleanup(){ docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT
docker run -d --name "$name" -p 127.0.0.1::9000 \
 --tmpfs /data:rw,size=8m,uid=1000,gid=1000,mode=0700 \
 -e STIVA_ACCESS_KEY="$AWS_ACCESS_KEY_ID" -e STIVA_SECRET_KEY="$AWS_SECRET_ACCESS_KEY" \
 "$STIVA_TEST_IMAGE" >/dev/null
port=$(docker port "$name" 9000/tcp | awk -F: '{print $NF}')
endpoint="http://127.0.0.1:$port"
ready=false
for _ in $(seq 1 60);do
 if curl -fsS "$endpoint/_ready" >/dev/null 2>&1;then ready=true;break;fi
 sleep 1
done
[ "$ready" = true ]
aws --endpoint-url "$endpoint" s3api create-bucket --bucket disk-full >/dev/null
# Random bytes and octet-stream prevent compression masking ENOSPC.
dd if=/dev/urandom of="$work/large" bs=1048576 count=12 2>/dev/null
if aws --endpoint-url "$endpoint" s3api put-object --bucket disk-full --key failed --body "$work/large" --content-type application/octet-stream >"$work/stdout" 2>"$work/stderr";then
 echo 'An upload larger than the data volume unexpectedly succeeded' >&2;exit 1
fi
# Check that the failure was actually ENOSPC, not an unrelated client error.
docker logs "$name" > "$work/server.log" 2>&1
if ! grep -qi 'no space left on device' "$work/server.log" "$work/stderr";then
 cat "$work/stderr" "$work/server.log" >&2;exit 1
fi
if aws --endpoint-url "$endpoint" s3api head-object --bucket disk-full --key failed >/dev/null 2>&1;then
 echo 'Failed upload published object metadata' >&2;exit 1
fi
# Failed temporary data must be removed, so a small upload can still commit.
printf 'healthy after ENOSPC\n' > "$work/small"
aws --endpoint-url "$endpoint" s3api put-object --bucket disk-full --key small --body "$work/small" >/dev/null
aws --endpoint-url "$endpoint" s3api get-object --bucket disk-full --key small "$work/recovered" >/dev/null
cmp "$work/small" "$work/recovered"
printf 'Disk-full recovery test passed\n'
