#!/usr/bin/env bash
set -euo pipefail
: "${STIVA_TEST_IMAGE:?Set STIVA_TEST_IMAGE}"
name="stiva-smoke-${RANDOM}"
work=$(mktemp -d)
export AWS_ACCESS_KEY_ID=smoke-access
AWS_SECRET_ACCESS_KEY=$(openssl rand -hex 32)
export AWS_SECRET_ACCESS_KEY
cleanup() {
 docker rm -f "$name" >/dev/null 2>&1 || true
 docker volume rm "$name" >/dev/null 2>&1 || true
 rm -rf "$work"
}
trap cleanup EXIT
docker volume create "$name" >/dev/null
docker run -d --name "$name" -p 127.0.0.1::9000 -p 127.0.0.1::9001 \
 -e STIVA_PRODUCTION=true -e STIVA_ACCESS_KEY="$AWS_ACCESS_KEY_ID" -e STIVA_SECRET_KEY="$AWS_SECRET_ACCESS_KEY" \
 -e STIVA_JWT_SECRET="$AWS_SECRET_ACCESS_KEY" -e STIVA_METRICS_TOKEN="$AWS_SECRET_ACCESS_KEY" \
 -v "$name:/data" "$STIVA_TEST_IMAGE" >/dev/null
port=$(docker port "$name" 9000/tcp | awk -F: '{print $NF}')
export STIVA_TEST_ENDPOINT="http://127.0.0.1:$port"
ready=false
for _ in $(seq 1 60); do
 if curl -fsS "$STIVA_TEST_ENDPOINT/_ready" >/dev/null 2>&1; then ready=true; break; fi
 sleep 1
done
if [ "$ready" != true ]; then docker logs "$name"; exit 1; fi
bash scripts/smoke-s3.sh
STIVA_LOAD_OBJECTS=32 STIVA_LOAD_WORKERS=4 bash scripts/load-s3.sh
# An acknowledged object must survive SIGKILL and container restart.
printf 'survive process crash\n' > "$work/crash"
bucket="crash-${RANDOM}"
aws --endpoint-url "$STIVA_TEST_ENDPOINT" s3api create-bucket --bucket "$bucket" >/dev/null
aws --endpoint-url "$STIVA_TEST_ENDPOINT" s3api put-object --bucket "$bucket" --key crash --body "$work/crash" >/dev/null
docker kill --signal KILL "$name" >/dev/null
docker start "$name" >/dev/null
port=$(docker port "$name" 9000/tcp | awk -F: '{print $NF}')
export STIVA_TEST_ENDPOINT="http://127.0.0.1:$port"
ready=false
for _ in $(seq 1 60); do
 if curl -fsS "$STIVA_TEST_ENDPOINT/_ready" >/dev/null 2>&1; then ready=true; break; fi
 sleep 1
done
[ "$ready" = true ]
aws --endpoint-url "$STIVA_TEST_ENDPOINT" s3api get-object --bucket "$bucket" --key crash "$work/recovered" >/dev/null
cmp "$work/crash" "$work/recovered"
printf 'Image and process crash tests passed\n'
