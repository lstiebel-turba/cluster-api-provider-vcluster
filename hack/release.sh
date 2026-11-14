#!/usr/bin/env bash
# Writes the release assets clusterctl and the Cluster API Operator read into RELEASE_DIR:
#   infrastructure-components.yaml                  config/release/default (helm installer)
#   infrastructure-components-flux.yaml             config/release/flux (--installer=flux)
#   infrastructure-components-flux-namespaced.yaml  config/release/flux-namespaced
#   metadata.yaml, cluster-template.yaml
# Usage: VERSION=v0.4.0-turba.1 IMAGE=ghcr.io/<owner>/<repo>:v0.4.0-turba.1@sha256:<digest> hack/release.sh
# (make release). IMAGE carries a tag, a digest or both; never "latest".
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${VERSION:?VERSION, for example v0.4.0-turba.1}"
IMAGE="${IMAGE:?IMAGE, for example ghcr.io/owner/cluster-api-provider-vcluster:$VERSION}"
RELEASE_DIR="${RELEASE_DIR:-$ROOT/out}"
KUSTOMIZE="${KUSTOMIZE:-kustomize}"
# The image config/default names; hack/release.sh replaces it.
BASE_IMAGE="ghcr.io/loft-sh/cluster-api-provider-vcluster"

die() {
  echo "release: $*" >&2
  exit 1
}

[[ "$VERSION" =~ ^v([0-9]+)\.([0-9]+)\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
  die "VERSION $VERSION is not v<major>.<minor>.<patch>[-<pre-release>]"
major="${BASH_REMATCH[1]}"
minor="${BASH_REMATCH[2]}"
# clusterctl reads the contract of a version from metadata.yaml's release series.
awk -v major="$major" -v minor="$minor" '
  $1 == "-" && $2 == "major:" { m = $3; next }
  $1 == "major:" { m = $2; next }
  $1 == "minor:" && m == major && $2 == minor { found = 1 }
  END { exit !found }' "$ROOT/metadata.yaml" ||
  die "metadata.yaml has no release series $major.$minor for $VERSION"

ref="$IMAGE"
digest=""
if [[ "$ref" == *@* ]]; then
  digest="${ref#*@}"
  ref="${ref%@*}"
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die "IMAGE digest $digest is not sha256:<64 hex>"
fi
name="$ref"
tag=""
if [[ "${ref##*/}" == *:* ]]; then
  name="${ref%:*}"
  tag="${ref##*:}"
fi
[[ -n "$tag" || -n "$digest" ]] || die "IMAGE $IMAGE has neither a tag nor a digest"
[[ "$tag" != latest ]] || die "IMAGE $IMAGE: no latest tag in a release"

# kustomize takes no absolute resource path: the image overlays live in bin/ (gitignored).
mkdir -p "$ROOT/bin"
work="$(mktemp -d "$ROOT/bin/release.XXXXXX")"
trap 'rm -rf "$work"' EXIT
mkdir -p "$RELEASE_DIR"

build() { # build <config/release overlay> <asset>
  mkdir -p "$work/$1"
  {
    echo "resources:"
    echo "- ../../../config/release/$1"
    echo "images:"
    echo "- name: $BASE_IMAGE"
    echo "  newName: $name"
    [[ -z "$tag" ]] || echo "  newTag: \"$tag\""
    [[ -z "$digest" ]] || echo "  digest: $digest"
  } >"$work/$1/kustomization.yaml"
  "$KUSTOMIZE" build "$work/$1" 2> >(grep -v '^# Warning' >&2) >"$RELEASE_DIR/$2"
}

build default infrastructure-components.yaml
build flux infrastructure-components-flux.yaml
build flux-namespaced infrastructure-components-flux-namespaced.yaml
cp "$ROOT/metadata.yaml" "$RELEASE_DIR/metadata.yaml"
cp "$ROOT/templates/cluster-template.yaml" "$RELEASE_DIR/cluster-template.yaml"
echo "release: $VERSION ($IMAGE) in $RELEASE_DIR"
