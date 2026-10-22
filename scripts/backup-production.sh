#!/usr/bin/env bash
# Run as the deployment operator. Destination must be on a separate mounted
# filesystem that is independently backed up/off-host (not the Stiva volume).
set -euo pipefail
: "${STIVA_ENV_FILE:?Absolute path to production.env}"
: "${STIVA_BACKUP_ROOT:?Absolute path to existing backup filesystem}"
: "${STIVA_DATA_PATH:?Absolute path to Stiva data directory}"
: "${STIVA_IMAGE:?Exact deployed Stiva image digest}"
: "${STIVA_TEXTFILE_DIR:?Node exporter textfile collector directory}"
repo=$(cd "$(dirname "$0")/.." && pwd)
compose() { docker compose --env-file "$STIVA_ENV_FILE" -f "$repo/deployment/compose.production.yml" "$@"; }
# A directory lock prevents overlapping scheduled runs.
lock="$STIVA_BACKUP_ROOT/.stiva-backup-lock"
mkdir "$lock"
restart=false
cleanup() {
 if [ "$restart" = true ]; then compose start stiva; fi
 rmdir "$lock"
}
trap cleanup EXIT
# Require a running service so cleanup preserves the previous operating state.
[ -n "$(compose ps --status running -q stiva)" ]
restart=true
compose stop stiva
stamp=$(date -u +%Y%m%dT%H%M%SZ)
name="stiva-$stamp"
docker run --rm --user "$(id -u):$(id -g)" \
 -v "$STIVA_DATA_PATH:/data:ro" -v "$STIVA_BACKUP_ROOT:/backup" \
 "$STIVA_IMAGE" backup --data-dir /data --output "/backup/$name"
# Verify using the same binary; publish freshness only after verification.
docker run --rm --user "$(id -u):$(id -g)" -v "$STIVA_BACKUP_ROOT:/backup:ro" \
 "$STIVA_IMAGE" verify-backup --source "/backup/$name"
compose start stiva
restart=false
metric=$(mktemp "$STIVA_TEXTFILE_DIR/.stiva-backup.XXXXXX")
printf 'stiva_backup_last_verified_timestamp_seconds %s\n' "$(date +%s)" > "$metric"
chmod 644 "$metric"
mv "$metric" "$STIVA_TEXTFILE_DIR/stiva-backup.prom"
printf 'Verified backup: %s/%s\n' "$STIVA_BACKUP_ROOT" "$name"
