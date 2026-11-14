#!/usr/bin/env bash
# End-to-end test of the flux installer on a kwok cluster: no Docker, no cloud.
#
#   hack/e2e-kwok.sh          # tools, then the scenario (make e2e-kwok)
#   hack/e2e-kwok.sh tools    # kwokctl, the Flux controllers and the Cluster API manager only
#   hack/e2e-kwok.sh down     # delete a cluster a kept run left (E2E_KWOK_KEEP=1)
#
# A kwokctl cluster with the binary runtime (a real kube-apiserver, etcd, scheduler and
# controller manager; kwok fakes the nodes and marks pods Running), and four local processes
# against it: Flux's source-controller and helm-controller, the Cluster API core manager
# (webhooks served but not registered), and this provider with --installer=flux.
#
# Scenario: a Cluster and its VCluster (the vCluster chart from VCLUSTER_CHART_REPO) ->
# Cluster API owns the VCluster -> the provider writes the HelmRepository and the HelmRelease
# -> helm-controller installs the chart (kwok's pods turn Ready) -> HelmChartDeployed True ->
# Cluster paused: HelmRelease suspended -> unpaused: resumed -> Cluster deleted: HelmRelease
# deleted, the Helm release uninstalled, the VCluster's finalizer gone.
# Not covered: the vCluster itself (its pods only look Running on kwok): no /readyz, no
# vc-<name> Secret, so KubeconfigReady, ControlPlaneInitialized and Ready stay False.
#
# Versions and checksums: hack/e2e-kwok.env. Downloads and builds: .cache/e2e-kwok (gitignored);
# logs: .cache/e2e-kwok/logs. E2E_KWOK_KEEP=1 keeps the cluster and the processes' logs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CACHE="${E2E_KWOK_CACHE:-$ROOT/.cache/e2e-kwok}"
BIN="$CACHE/bin"
LOGS="$CACHE/logs"
CLUSTER="${E2E_KWOK_CLUSTER:-capvc-e2e-kwok}"
NS=e2e
NAME=vc1
export KWOK_WORKDIR="$CACHE/work"

# hack/e2e-kwok.env; the environment wins
while IFS='=' read -r key value; do
  [[ "$key" =~ ^[A-Z][A-Z0-9_]*$ ]] || continue
  [[ -n "${!key:-}" ]] || export "$key=$value"
done <"$ROOT/hack/e2e-kwok.env"
export KWOK_KUBE_VERSION="v${KWOK_KUBE_VERSION#v}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *)
    echo "e2e-kwok: no kwokctl build for $(uname -m)" >&2
    exit 2
    ;;
esac

started=$SECONDS
log() { printf '[e2e-kwok %4ds] %s\n' "$((SECONDS - started))" "$*" >&2; }

sha256() { (sha256sum "$1" 2>/dev/null || shasum -a 256 "$1") | cut -d' ' -f1; }

# fetch URL DEST SHA256: downloads once, refuses a checksum mismatch
fetch() {
  local url="$1" dest="$2" want="$3" tmp="$2.download.$$" got
  [[ -s "$dest" ]] && return 0
  mkdir -p "$(dirname "$dest")"
  curl -fsSL --retry 3 -o "$tmp" "$url"
  got="$(sha256 "$tmp")"
  if [[ "$got" != "$want" ]]; then
    rm -f "$tmp"
    echo "e2e-kwok: $url: sha256 $got, hack/e2e-kwok.env pins $want" >&2
    exit 2
  fi
  mv "$tmp" "$dest"
}

# build NAME OUT URL SHA256 [PKG]: a Go binary from a source tarball, once per version
build_from_source() {
  local name="$1" out="$2" url="$3" sum="$4" pkg="${5:-.}" src
  [[ -x "$out" ]] && return 0
  log "building $name from $url"
  fetch "$url" "$CACHE/src/$(basename "$out").tar.gz" "$sum"
  src="$(mktemp -d)"
  tar -xzf "$CACHE/src/$(basename "$out").tar.gz" -C "$src"
  (cd "$src"/*/ && CGO_ENABLED=0 GOFLAGS=-trimpath go build -o "$out" "$pkg")
  rm -rf "$src"
}

tools() {
  mkdir -p "$BIN" "$CACHE/crds" "$KWOK_WORKDIR" "$LOGS"
  local sum_var
  sum_var="KWOK_SHA256_KWOKCTL_$(tr '[:lower:]' '[:upper:]' <<<"${os}_$arch")"
  fetch "https://github.com/kubernetes-sigs/kwok/releases/download/v$KWOK_VERSION/kwokctl-$os-$arch" \
    "$BIN/kwokctl-$KWOK_VERSION" "${!sum_var:?hack/e2e-kwok.env has no $sum_var}"
  chmod +x "$BIN/kwokctl-$KWOK_VERSION"
  ln -sf "kwokctl-$KWOK_VERSION" "$BIN/kwokctl"
  export KWOK_BIN="$BIN"

  local c v
  for c in helm source; do
    v="FLUX_$(tr '[:lower:]' '[:upper:]' <<<"$c")_CONTROLLER"
    local version="${v}_VERSION" src_sum="${v}_SHA256_SRC" crd_sum="${v}_SHA256_CRDS"
    build_from_source "$c-controller" "$BIN/$c-controller-${!version}" \
      "https://github.com/fluxcd/$c-controller/archive/refs/tags/v${!version}.tar.gz" "${!src_sum}"
    fetch "https://github.com/fluxcd/$c-controller/releases/download/v${!version}/$c-controller.crds.yaml" \
      "$CACHE/crds/$c-controller-${!version}.yaml" "${!crd_sum}"
  done

  # the Cluster API manager and its CRDs from the same source tree
  if [[ ! -x "$BIN/capi-manager-$CAPI_VERSION" || ! -d "$CACHE/crds/capi-$CAPI_VERSION" ]]; then
    build_from_source cluster-api "$BIN/capi-manager-$CAPI_VERSION" \
      "https://github.com/kubernetes-sigs/cluster-api/archive/refs/tags/$CAPI_VERSION.tar.gz" "$CAPI_SHA256_SRC"
    local src
    src="$(mktemp -d)"
    tar -xzf "$CACHE/src/capi-manager-$CAPI_VERSION.tar.gz" -C "$src"
    rm -rf "$CACHE/crds/capi-$CAPI_VERSION"
    cp -R "$src"/*/config/crd/bases "$CACHE/crds/capi-$CAPI_VERSION"
    rm -rf "$src"
  fi

  log "building the provider"
  (cd "$ROOT" && CGO_ENABLED=0 go build -o "$BIN/capvc-manager" .)
}

kwokctl() { "$BIN/kwokctl" "$@"; }

down() {
  [[ -x "$BIN/kwokctl" ]] || return 0
  if kwokctl get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    kwokctl delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    log "deleted cluster $CLUSTER"
  fi
}

pids=()
names=()
cleanup() {
  local rc=$?
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  for pid in "${pids[@]}"; do
    wait "$pid" 2>/dev/null || true
  done
  if [[ "${E2E_KWOK_KEEP:-}" == 1 ]]; then
    log "kept cluster $CLUSTER (KUBECONFIG=${KUBECONFIG:-}; hack/e2e-kwok.sh down deletes it)"
  else
    down
  fi
  if ((rc == 0)); then
    log "PASS in $((SECONDS - started))s"
  else
    log "FAIL (exit $rc); logs in $LOGS"
  fi
}

# start NAME CMD...: a local process against the cluster, logging to $LOGS/NAME.log
start() {
  local name="$1"
  shift
  "$@" >"$LOGS/$name.log" 2>&1 &
  pids+=("$!")
  names+=("$name")
  log "started $name (pid $!, $LOGS/$name.log)"
}

# a free port below the kernels' ephemeral ranges, for source-controller's artifact server
free_port() {
  local p
  for _ in $(seq 50); do
    p=$((20000 + RANDOM % 12000))
    if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
      echo "$p"
      return 0
    fi
  done
  echo "e2e-kwok: no free port in 20000-31999" >&2
  return 1
}

k() { kubectl "$@"; }
VC=vclusters.infrastructure.cluster.x-k8s.io
HR=helmreleases.helm.toolkit.fluxcd.io
HREPO=helmrepositories.source.toolkit.fluxcd.io
CL=clusters.cluster.x-k8s.io

# jp KIND NAME JSONPATH: a field of an object in $NS, empty if the object is missing
jp() { k get "$1" -n "$NS" "$2" -o "jsonpath=$3" 2>/dev/null || true; }
# expect KIND NAME JSONPATH WANT: the field equals WANT
expect() { [[ "$(jp "$1" "$2" "$3")" == "$4" ]]; }
# missing KIND NAME: the object no longer exists
missing() { [[ -z "$(k get "$1" -n "$NS" "$2" --ignore-not-found -o name)" ]]; }
# no_release_secret: Helm keeps no release record of $NAME (Secret metadata only)
no_release_secret() { [[ -z "$(k get secret -n "$NS" -l owner=helm,name="$NAME" -o name)" ]]; }
condition() { expect "$1" "$2" "{.status.conditions[?(@.type==\"$3\")].status}" "$4"; }

dump() {
  log "state at the failure:"
  k get "$CL,$VC,$HREPO,$HR,deploy,pods" -n "$NS" -o wide >&2 || true
  k get "$VC,$HR" -n "$NS" -o yaml >"$LOGS/objects.yaml" 2>&1 || true
  for f in "$LOGS"/*.log; do
    echo "--- tail $f" >&2
    tail -n 15 "$f" >&2
  done
}

# alive: every local process still runs (one that exited fails the run with its log)
alive() {
  local i
  for i in "${!pids[@]}"; do
    if ! kill -0 "${pids[$i]}" 2>/dev/null; then
      log "FAIL: ${names[$i]} exited: $LOGS/${names[$i]}.log"
      dump
      exit 1
    fi
  done
}

# wait_for DESCRIPTION TIMEOUT_SECONDS CHECK...: CHECK until it succeeds, or fail
wait_for() {
  local desc="$1" timeout="$2" t0=$SECONDS
  shift 2
  until alive && "$@"; do
    if ((SECONDS - t0 >= timeout)); then
      log "FAIL: $desc: not within ${timeout}s"
      dump
      exit 1
    fi
    sleep 1
  done
  log "ok: $desc ($((SECONDS - t0))s)"
}

# check DESCRIPTION CHECK...: CHECK now, or fail
check() {
  local desc="$1"
  shift
  if ! "$@"; then
    log "FAIL: $desc"
    dump
    exit 1
  fi
  log "ok: $desc"
}

scenario() {
  command -v kubectl >/dev/null || {
    echo "e2e-kwok: needs kubectl" >&2
    exit 2
  }
  tools
  log "tools ready"
  down
  trap cleanup EXIT
  trap "exit 130" INT
  trap "exit 143" TERM

  # kwokctl's --kubeconfig: a file of its own, never ~/.kube/config
  kwokctl create cluster --name "$CLUSTER" --runtime binary \
    --kubeconfig "$KWOK_WORKDIR/$CLUSTER.kubeconfig" --wait 2m >"$LOGS/kwokctl-create.log" 2>&1 ||
    {
      cat "$LOGS/kwokctl-create.log" >&2
      exit 1
    }
  export KUBECONFIG="$KWOK_WORKDIR/clusters/$CLUSTER/kubeconfig.yaml"
  log "cluster $CLUSTER up (Kubernetes $KWOK_KUBE_VERSION)"

  k apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Node
metadata:
  name: kwok-node-0
  annotations:
    kwok.x-k8s.io/node: fake
  labels:
    kubernetes.io/hostname: kwok-node-0
    type: kwok
status:
  allocatable: {cpu: "32", memory: 128Gi, ephemeral-storage: 500Gi, pods: "110"}
  capacity: {cpu: "32", memory: 128Gi, ephemeral-storage: 500Gi, pods: "110"}
EOF
  k apply --server-side -f "$CACHE/crds/helm-controller-$FLUX_HELM_CONTROLLER_VERSION.yaml" \
    -f "$CACHE/crds/source-controller-$FLUX_SOURCE_CONTROLLER_VERSION.yaml" \
    -f "$CACHE/crds/capi-$CAPI_VERSION" >/dev/null
  k apply --server-side -k "$ROOT/config/crd" >/dev/null
  k wait --for=condition=Established --timeout=60s crd --all >/dev/null
  k create namespace flux-system >/dev/null
  k create namespace "$NS" >/dev/null
  log "CRDs established: Flux, Cluster API, the provider's"

  local storage certs
  storage="$(free_port)"
  rm -rf "$CACHE/source-controller-data"
  mkdir -p "$CACHE/source-controller-data"
  start source-controller env RUNTIME_NAMESPACE=flux-system "$BIN/source-controller-$FLUX_SOURCE_CONTROLLER_VERSION" \
    --watch-all-namespaces --log-encoding=console --storage-path="$CACHE/source-controller-data" \
    --storage-addr="127.0.0.1:$storage" --storage-adv-addr="127.0.0.1:$storage" \
    --metrics-addr=127.0.0.1:0 --health-addr=127.0.0.1:0
  start helm-controller env RUNTIME_NAMESPACE=flux-system "$BIN/helm-controller-$FLUX_HELM_CONTROLLER_VERSION" \
    --watch-all-namespaces --log-encoding=console --metrics-addr=127.0.0.1:0 --health-addr=127.0.0.1:0

  # the Cluster API manager serves its webhooks (nothing calls them: no webhook configurations)
  certs="$CACHE/capi-webhook-certs"
  if [[ ! -s "$certs/tls.crt" ]]; then
    mkdir -p "$certs"
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj /CN=localhost \
      -keyout "$certs/tls.key" -out "$certs/tls.crt" >/dev/null 2>&1
  fi
  start capi-manager "$BIN/capi-manager-$CAPI_VERSION" --webhook-port="$(free_port)" --webhook-cert-dir="$certs" \
    --health-addr=127.0.0.1:0 --diagnostics-address=127.0.0.1:0 --insecure-diagnostics
  start provider "$BIN/capvc-manager" --installer=flux --namespace="$NS" \
    --metrics-bind-address=0 --health-probe-bind-address=0

  # 1. a Cluster and its VCluster
  k apply -n "$NS" -f - >/dev/null <<EOF
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: VCluster
metadata:
  name: $NAME
spec:
  helmRelease:
    chart:
      repo: $VCLUSTER_CHART_REPO
      name: vcluster
      version: "$VCLUSTER_CHART_VERSION"
    values: |
      controlPlane:
        statefulSet:
          persistence:
            volumeClaim:
              enabled: false
---
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: $NAME
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: VCluster
    name: $NAME
  controlPlaneRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: VCluster
    name: $NAME
EOF
  log "created Cluster and VCluster $NS/$NAME"

  wait_for "Cluster API owns the VCluster" 60 \
    expect "$VC" "$NAME" '{.metadata.ownerReferences[?(@.kind=="Cluster")].name}' "$NAME"
  wait_for "the VCluster has the provider's finalizer" 60 \
    expect "$VC" "$NAME" '{.metadata.finalizers}' '["vcluster.loft.sh/cleanup"]'
  wait_for "HelmRelease $NAME written" 60 expect "$HR" "$NAME" '{.metadata.name}' "$NAME"

  # 2. the HelmRepository and the HelmRelease as the provider writes them
  check "HelmRepository url" expect "$HREPO" "$NAME" '{.spec.url}' "$VCLUSTER_CHART_REPO"
  check "HelmRepository owned by the VCluster" \
    expect "$HREPO" "$NAME" '{.metadata.ownerReferences[?(@.controller==true)].kind}' VCluster
  check "HelmRelease owned by the VCluster" \
    expect "$HR" "$NAME" '{.metadata.ownerReferences[?(@.controller==true)].name}' "$NAME"
  check "HelmRelease releaseName" expect "$HR" "$NAME" '{.spec.releaseName}' "$NAME"
  check "HelmRelease chart" expect "$HR" "$NAME" '{.spec.chart.spec.chart}' vcluster
  check "HelmRelease chart version" expect "$HR" "$NAME" '{.spec.chart.spec.version}' "$VCLUSTER_CHART_VERSION"
  check "HelmRelease sourceRef" expect "$HR" "$NAME" '{.spec.chart.spec.sourceRef.kind}/{.spec.chart.spec.sourceRef.name}' "HelmRepository/$NAME"
  check "HelmRelease values" \
    expect "$HR" "$NAME" '{.spec.values.controlPlane.statefulSet.persistence.volumeClaim.enabled}' false
  check "HelmRelease driftDetection" expect "$HR" "$NAME" '{.spec.driftDetection.mode}' enabled
  check "HelmRelease not suspended" expect "$HR" "$NAME" '{.spec.suspend}' false

  # 3. helm-controller installs the chart; kwok marks its pods Ready
  wait_for "HelmRepository Ready (source-controller fetched the index)" 180 condition "$HREPO" "$NAME" Ready True
  wait_for "HelmRelease Ready (helm-controller installed the chart)" 300 condition "$HR" "$NAME" Ready True
  check "the chart's Deployment is there" expect deploy "$NAME" '{.metadata.name}' "$NAME"
  wait_for "VCluster HelmChartDeployed True" 60 condition "$VC" "$NAME" HelmChartDeployed True

  # 4. pause and unpause the Cluster
  k patch "$CL" -n "$NS" "$NAME" --type merge -p '{"spec":{"paused":true}}' >/dev/null
  wait_for "Cluster paused: VCluster Paused True" 60 condition "$VC" "$NAME" Paused True
  wait_for "Cluster paused: HelmRelease suspended" 60 expect "$HR" "$NAME" '{.spec.suspend}' true
  k patch "$CL" -n "$NS" "$NAME" --type merge -p '{"spec":{"paused":false}}' >/dev/null
  wait_for "Cluster unpaused: VCluster Paused False" 60 condition "$VC" "$NAME" Paused False
  wait_for "Cluster unpaused: HelmRelease resumed" 60 expect "$HR" "$NAME" '{.spec.suspend}' false

  # 5. delete the Cluster: Cluster API deletes the VCluster, the provider the HelmRelease
  k delete "$CL" -n "$NS" "$NAME" --wait=false >/dev/null
  wait_for "HelmRelease deleted" 180 missing "$HR" "$NAME"
  wait_for "Helm release uninstalled (no release Secret)" 60 \
    no_release_secret
  wait_for "the chart's Deployment deleted" 60 missing deploy "$NAME"
  wait_for "VCluster gone (finalizer removed)" 60 missing "$VC" "$NAME"
  wait_for "HelmRepository gone (owner reference)" 60 missing "$HREPO" "$NAME"
  wait_for "Cluster gone" 60 missing "$CL" "$NAME"
}

case "${1:-run}" in
  run) scenario ;;
  tools) tools ;;
  down) down ;;
  *)
    echo "usage: $0 [run|tools|down]" >&2
    exit 2
    ;;
esac
