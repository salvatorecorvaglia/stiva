#!/usr/bin/env bash
set -euo pipefail
refs=()
for arch in amd64 arm64; do
  directory="verified-images/verified-image-$arch"
  (cd "$directory" && sha256sum --check SHA256SUMS)
  test "$(cat "$directory/commit")" = "$GITHUB_SHA"
  skopeo --override-arch "$arch" copy "oci-archive:$directory/image.oci.tar" docker-daemon:stiva:ci
  image_id="$(cat "$directory/image-id")"
  test "$(docker image inspect stiva:ci --format '{{.Id}}')" = "$image_id"
  test "$(docker image inspect "$image_id" --format '{{.Os}}/{{.Architecture}}')" = "linux/$arch"
  ref="$IMAGE_NAME:verified-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT-$arch"
  # Copy the complete OCI index, including SBOM and provenance attestations.
  skopeo copy --all --preserve-digests --digestfile "$directory/published-digest" \
    "oci-archive:$directory/image.oci.tar" "docker://$ref"
  digest="$(cat "$directory/published-digest")"
  [[ "$digest" == sha256:* ]]
  refs+=("$IMAGE_NAME@$digest")
done
tags=()
while IFS= read -r tag; do
  if [[ -n "$tag" ]]; then tags+=(--tag "$tag"); fi
done <<< "$RELEASE_TAGS"
test "${#tags[@]}" -gt 0
docker buildx imagetools create "${tags[@]}" "${refs[@]}"
for ref in "${refs[@]}"; do printf '%s\n' "$ref" >> "$GITHUB_STEP_SUMMARY"; done
