#!/usr/bin/env bash
# Usage: hack/image-manifest.sh <image> <digests dir> <tag>...
# Tags one manifest list of the per-architecture images .github/workflows/image.yaml pushed by
# digest (one file per architecture in <digests dir>, holding sha256:...) with each <tag>, and
# prints the list's digest.
set -euo pipefail

image="${1:?image}"
dir="${2:?digests dir}"
shift 2
(($# > 0)) || {
  echo "image-manifest: no tag" >&2
  exit 1
}

refs=()
for arch in amd64 arm64; do
  refs+=("$image@$(cat "$dir/$arch")")
done
tags=()
for tag in "$@"; do
  tags+=(-t "$image:$tag")
done
docker buildx imagetools create "${tags[@]}" "${refs[@]}" >&2
docker buildx imagetools inspect "$image:$1" >&2
docker buildx imagetools inspect "$image:$1" --format '{{json .Manifest}}' | jq -er .digest
