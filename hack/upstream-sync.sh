#!/usr/bin/env bash
# Lists the upstream (loft-sh/cluster-api-provider-vcluster) commits this fork has neither merged
# nor skipped (docs/fork.md, "Upstream sync"). Read-only apart from the fetch.
#
# Usage:
#   hack/upstream-sync.sh [list] [REF]       fetch upstream, list what REF (default HEAD) lacks;
#                                            exit 1 when anything is listed
#   hack/upstream-sync.sh skip SHA REASON... add an upstream commit to hack/upstream-skipped.txt
#
# A commit counts as merged when REF contains it or a patch-identical copy (git cherry-pick).
# One picked with changes, or one the fork doesn't want, goes in hack/upstream-skipped.txt:
# "<full sha> <reason>", one per line.
#
# Environment: UPSTREAM_URL (default https://github.com/loft-sh/cluster-api-provider-vcluster.git,
# used when there is no git remote "upstream"), UPSTREAM_BRANCH (default main).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SKIPPED="$ROOT/hack/upstream-skipped.txt"
UPSTREAM_URL="${UPSTREAM_URL:-https://github.com/loft-sh/cluster-api-provider-vcluster.git}"
UPSTREAM_BRANCH="${UPSTREAM_BRANCH:-main}"
UPSTREAM_REF="refs/remotes/upstream/$UPSTREAM_BRANCH"

die() {
  echo "upstream-sync: $*" >&2
  exit 2
}

fetch() {
  if git -C "$ROOT" remote get-url upstream >/dev/null 2>&1; then
    git -C "$ROOT" fetch --quiet --no-tags upstream "+refs/heads/$UPSTREAM_BRANCH:$UPSTREAM_REF"
  else
    git -C "$ROOT" fetch --quiet --no-tags "$UPSTREAM_URL" "+refs/heads/$UPSTREAM_BRANCH:$UPSTREAM_REF"
  fi
}

# skipped: the full shas of hack/upstream-skipped.txt
skipped() {
  [[ -f "$SKIPPED" ]] || return 0
  sed -E 's/#.*//' "$SKIPPED" | awk 'NF { print $1 }'
}

list() {
  local ref="${1:-HEAD}" sha date subject pending=0
  git -C "$ROOT" rev-parse --verify --quiet "$ref^{commit}" >/dev/null || die "no commit $ref"
  # A shallow clone has no merge base: the whole history is needed.
  [[ "$(git -C "$ROOT" rev-parse --is-shallow-repository)" == false ]] || die "shallow clone: fetch the full history"
  fetch
  local skips
  skips="$(skipped)"
  echo "upstream $UPSTREAM_BRANCH $(git -C "$ROOT" rev-parse --short=12 "$UPSTREAM_REF"), fork $ref $(git -C "$ROOT" rev-parse --short=12 "$ref")"
  while IFS=$'\t' read -r sha date subject; do
    [[ -n "$sha" ]] || continue
    grep -qx "$sha" <<<"$skips" && continue
    echo "  $sha  $date  $subject"
    pending=$((pending + 1))
  done < <(git -C "$ROOT" log --no-merges --cherry-pick --right-only --reverse \
    --format=$'%H\t%cs\t%s' "$ref...$UPSTREAM_REF")
  if ((pending > 0)); then
    echo "$pending upstream commit(s) neither merged nor in hack/upstream-skipped.txt"
    return 1
  fi
  echo "nothing new upstream"
}

skip() {
  local sha="${1:-}"
  shift || true
  [[ -n "$sha" && $# -gt 0 ]] || die "usage: $0 skip SHA REASON..."
  sha="$(git -C "$ROOT" rev-parse --verify --quiet "$sha^{commit}")" || die "no commit $1 (fetch first: $0 list)"
  git -C "$ROOT" merge-base --is-ancestor "$sha" "$UPSTREAM_REF" || die "$sha is not on upstream $UPSTREAM_BRANCH"
  ! skipped | grep -qx "$sha" || die "$sha is already skipped"
  echo "$sha $*" >>"$SKIPPED"
  echo "skipped $sha: $*"
}

case "${1:-list}" in
  list)
    shift || true
    list "$@"
    ;;
  skip)
    shift
    skip "$@"
    ;;
  -h | --help) sed -n '2,15p' "$0" ;;
  *) die "unknown command $1 (list, skip)" ;;
esac
