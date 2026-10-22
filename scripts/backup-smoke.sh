#!/usr/bin/env bash
# End-to-end offline backup/restore through the container's actual CLI.
set -euo pipefail
: "${STIVA_TEST_IMAGE:?Required}"
name="stiva-backup-${RANDOM}"
work=$(mktemp -d)
export AWS_ACCESS_KEY_ID=backup-smoke-access
AWS_SECRET_ACCESS_KEY=$(openssl rand -hex 32)
export AWS_SECRET_ACCESS_KEY AWS_EC2_METADATA_DISABLED=true AWS_DEFAULT_REGION=us-east-1
cleanup(){
 docker rm -f "$name" >/dev/null 2>&1 || true
 docker volume rm "$name-data" "$name-backup" "$name-restore" >/dev/null 2>&1 || true
 rm -rf "$work"
}
trap cleanup EXIT
for suffix in data backup restore;do docker volume create "$name-$suffix" >/dev/null;done
start(){
 docker run -d --name "$name" -p 127.0.0.1::9000 \
 -e STIVA_ACCESS_KEY="$AWS_ACCESS_KEY_ID" -e STIVA_SECRET_KEY="$AWS_SECRET_ACCESS_KEY" \
 -v "$1:/data" "$STIVA_TEST_IMAGE" >/dev/null
 port=$(docker port "$name" 9000/tcp | awk -F: '{print $NF}')
 endpoint="http://127.0.0.1:$port"
 ready=false
 for _ in $(seq 1 60);do
  if curl -fsS "$endpoint/_ready" >/dev/null 2>&1;then ready=true;break;fi
  sleep 1
 done
 [ "$ready" = true ]
}
start "$name-data"
printf 'restore this acknowledged object\n' > "$work/source"
aws --endpoint-url "$endpoint" s3api create-bucket --bucket restore-test >/dev/null
aws --endpoint-url "$endpoint" s3api put-object --bucket restore-test --key object --body "$work/source" >/dev/null
docker stop "$name" >/dev/null
docker rm "$name" >/dev/null
# Backup/restore tools run as root only to initialize these disposable volumes;
# the server itself runs as the image's non-root UID.
docker run --rm --user 0 -v "$name-data:/data:ro" -v "$name-backup:/backup" \
 "$STIVA_TEST_IMAGE" backup --data-dir /data --output /backup/snapshot
docker run --rm --user 0 -v "$name-backup:/backup:ro" \
 "$STIVA_TEST_IMAGE" verify-backup --source /backup/snapshot
docker run --rm --user 0 -v "$name-backup:/backup:ro" -v "$name-restore:/restore" \
 "$STIVA_TEST_IMAGE" restore --source /backup/snapshot --output /restore/recovered
docker run --rm --user 0 --entrypoint chown -v "$name-restore:/restore" "$STIVA_TEST_IMAGE" -R 1000:1000 /restore/recovered
# The restore is a new directory; mount its volume and select that directory.
docker run -d --name "$name" -p 127.0.0.1::9000 \
 -e STIVA_DATA_DIR=/restore/recovered -e STIVA_ACCESS_KEY="$AWS_ACCESS_KEY_ID" -e STIVA_SECRET_KEY="$AWS_SECRET_ACCESS_KEY" \
 -v "$name-restore:/restore" "$STIVA_TEST_IMAGE" >/dev/null
port=$(docker port "$name" 9000/tcp | awk -F: '{print $NF}')
endpoint="http://127.0.0.1:$port"
ready=false
for _ in $(seq 1 60);do
 if curl -fsS "$endpoint/_ready" >/dev/null 2>&1;then ready=true;break;fi
 sleep 1
done
[ "$ready" = true ]
aws --endpoint-url "$endpoint" s3api get-object --bucket restore-test --key object "$work/restored" >/dev/null
cmp "$work/source" "$work/restored"
printf 'Container backup and restore drill passed\n'
