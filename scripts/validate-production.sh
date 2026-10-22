#!/usr/bin/env bash
set -euo pipefail
: "${STIVA_ENV_FILE:?Set absolute path to production.env}"
repo=$(cd "$(dirname "$0")/.." && pwd)
docker compose --env-file "$STIVA_ENV_FILE" -f "$repo/deployment/compose.production.yml" config --format json | python3 -c '
import json,re,sys
config=json.load(sys.stdin)
for name in ("stiva","proxy"):
 image=config["services"][name]["image"]
 if not re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}",image):
  sys.exit(name+": production requires an immutable sha256 image digest")
env=config["services"]["stiva"]["environment"]
secrets=[env.get(k,"") for k in ("STIVA_SECRET_KEY","STIVA_JWT_SECRET","STIVA_METRICS_TOKEN")]
if any(len(value)<32 or "REPLACE" in value for value in secrets) or len(set(secrets))!=3:
 sys.exit("production requires three independent secrets of at least 32 characters")
if env.get("STIVA_ACCESS_KEY") in ("stiva","REPLACE_WITH_CUSTOM_ACCESS_KEY",""):
 sys.exit("set a custom access key")
print("Production configuration valid; no secrets printed")
'
