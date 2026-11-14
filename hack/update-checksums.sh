#!/usr/bin/env bash
# Rewrites every sha256 pin to match the version next to it: Helm in the Dockerfile, kind in
# .github/workflows/e2e-kind.yaml, kwokctl, the Flux controllers and CAPI in hack/e2e-kwok.env.
# Run it on a Renovate branch that moved one of those versions, then commit the result.
# Downloads each artifact once (public GitHub and get.helm.sh), nothing else.
#
# Usage: hack/update-checksums.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# sum URL: the sha256 of the download
sum() {
  curl -fsSL --retry 3 "$1" -o "$TMP/dl"
  sha256 "$TMP/dl"
}

# pin FILE KEY SUM: replaces the 64-hex value of `KEY=`, `ARG KEY=` or `KEY: ` in FILE
pin() {
  local file="$ROOT/$1" key="$2" value="$3"
  grep -Eq "^[[:space:]]*(ARG )?$key(=|: )" "$file" || { echo "update-checksums: $1 has no $key" >&2; exit 1; }
  KEY="$key" VALUE="$value" perl -pi -e 's/^(\s*(?:ARG )?\Q$ENV{KEY}\E(?:=|: ))[0-9a-f]{64}/$1$ENV{VALUE}/' "$file"
  echo "$1 $key=$value"
}

# value FILE KEY: the value of `KEY=`, `ARG KEY=` or `KEY: ` in FILE, quotes and comment stripped
value() {
  sed -nE "s/^[[:space:]]*(ARG )?$2(=|: )\"?([^\" #]+).*/\\3/p" "$ROOT/$1" | head -n1
}

# Dockerfile: Helm
helm="$(value Dockerfile HELM_VERSION)"
for arch in amd64 arm64; do
  pin Dockerfile "HELM_SHA256_$arch" "$(sum "https://get.helm.sh/helm-$helm-linux-$arch.tar.gz")"
done

# e2e-kind.yaml: kind
kind="$(value .github/workflows/e2e-kind.yaml KIND_VERSION)"
pin .github/workflows/e2e-kind.yaml KIND_SHA256 \
  "$(sum "https://github.com/kubernetes-sigs/kind/releases/download/$kind/kind-linux-amd64")"

# e2e-kwok.env: the URLs hack/e2e-kwok.sh downloads
env=hack/e2e-kwok.env
kwok="$(value $env KWOK_VERSION)"
for os in linux darwin; do
  for arch in amd64 arm64; do
    key="KWOK_SHA256_KWOKCTL_$(tr '[:lower:]' '[:upper:]' <<<"${os}_$arch")"
    pin $env "$key" "$(sum "https://github.com/kubernetes-sigs/kwok/releases/download/v$kwok/kwokctl-$os-$arch")"
  done
done
for c in helm source; do
  key="FLUX_$(tr '[:lower:]' '[:upper:]' <<<"$c")_CONTROLLER"
  v="$(value $env "${key}_VERSION")"
  pin $env "${key}_SHA256_SRC" "$(sum "https://github.com/fluxcd/$c-controller/archive/refs/tags/v$v.tar.gz")"
  pin $env "${key}_SHA256_CRDS" "$(sum "https://github.com/fluxcd/$c-controller/releases/download/v$v/$c-controller.crds.yaml")"
done
capi="$(value $env CAPI_VERSION)"
pin $env CAPI_SHA256_SRC "$(sum "https://github.com/kubernetes-sigs/cluster-api/archive/refs/tags/$capi.tar.gz")"
