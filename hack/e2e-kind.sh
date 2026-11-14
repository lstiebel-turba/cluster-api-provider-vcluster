#!/usr/bin/env bash
# End-to-end test of the provider on kind with a real vCluster: Cluster API core, the provider
# image built from this checkout, and (installer flux) Flux's source- and helm-controller.
#
#   hack/e2e-kind.sh helm|flux        (make e2e-kind INSTALLER=helm|flux)
#
# Scenarios, per installer:
#   1. create  Cluster + VCluster → Available, provisioned, Secret <name>-kubeconfig answers
#   2. park    Cluster paused, the control plane and etcd scaled to 0, held for PARK_SECONDS;
#      wake    unpaused, scaled back, the API answers and a ConfigMap written before survived
#   3. delete  Cluster gone → VCluster, release and workloads gone, the namespace deletes
#   4. adopt   (flux) a hand-written HelmRelease <name>, as Turbacharger's controller writes it
#              today, suspended and taken over by a VCluster of the same name: the control plane
#              keeps running (same pod) and its data
#
# Environment:
#   KIND_CLUSTER       kind cluster name (default capvc-e2e); reused if it exists
#   KEEP_CLUSTER=1     keep the kind cluster afterwards (default: deleted on success only)
#   CONTAINER_ENGINE   docker or podman (default: docker if present, else podman); with podman,
#                      kind needs KIND_EXPERIMENTAL_PROVIDER=podman
#   ARTIFACTS          where diagnostics go on failure (default: a temp dir)
#   PARK_SECONDS       how long the parked vCluster must stay at zero (default 60)
#   SKIP_BUILD=1       don't rebuild the provider image (it must be loaded already)
set -Eeuo pipefail

INSTALLER="${1:-${INSTALLER:-helm}}"
case "$INSTALLER" in helm | flux) ;; *)
  echo "usage: $0 helm|flux" >&2
  exit 2
  ;;
esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Pinned versions
# renovate: datasource=docker depName=kindest/node
KIND_NODE_IMAGE="kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0" # kind v0.33.0
# renovate: datasource=github-releases depName=cert-manager/cert-manager
CERT_MANAGER_VERSION="v1.20.5"
# renovate: datasource=github-releases depName=kubernetes-sigs/cluster-api
CAPI_VERSION="v1.13.8"
# renovate: datasource=github-releases depName=fluxcd/source-controller
SOURCE_CONTROLLER_VERSION="v1.9.6"
# renovate: datasource=github-releases depName=fluxcd/helm-controller
HELM_CONTROLLER_VERSION="v1.6.5"
# renovate: datasource=github-releases depName=prometheus-operator/prometheus-operator
PROMETHEUS_OPERATOR_VERSION="v0.95.2" # ServiceMonitor CRD only
# renovate: datasource=helm depName=vcluster registryUrl=https://charts.loft.sh
VCLUSTER_CHART_VERSION="0.37.6"
VCLUSTER_CHART_REPO="https://charts.loft.sh"

PROVIDER_IMAGE="capvc.local/cluster-api-provider-vcluster:e2e" # test/e2e/kind/provider
PRIORITY_CLASS="turba-env-tenant-system"
MONITOR_TLS_ANNOTATION="turba-env.dev/monitor-tls"

KIND_CLUSTER="${KIND_CLUSTER:-capvc-e2e}"
PARK_SECONDS="${PARK_SECONDS:-60}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/capvc-e2e.XXXXXX")"
ARTIFACTS="${ARTIFACTS:-$WORK/artifacts}"
mkdir -p "$ARTIFACTS"
export KUBECONFIG="$WORK/kind.kubeconfig" # never ~/.kube/config
if [[ -z "${CONTAINER_ENGINE:-}" ]]; then
  if command -v docker >/dev/null 2>&1; then CONTAINER_ENGINE=docker; else CONTAINER_ENGINE=podman; fi
fi

PF_PID=""
PF_PORT=""
CURRENT_NS=""
RESULTS=()

log() { printf '\n[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
pass() {
  RESULTS+=("PASS  $INSTALLER  $*")
  printf '  PASS %s\n' "$*"
}
note() {
  RESULTS+=("NOTE  $INSTALLER  $*")
  printf '  NOTE %s\n' "$*"
}
fail() {
  RESULTS+=("FAIL  $INSTALLER  $*")
  printf '  FAIL %s\n' "$*" >&2
  exit 1
}

# wait_until <seconds> <description> <command...>: polls every 3 s
wait_until() {
  local timeout="$1" what="$2"
  shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    if ((SECONDS >= deadline)); then fail "timed out after ${timeout}s: $what"; fi
    sleep 3
  done
  pass "$what"
}

jp() { kubectl get "$@" 2>/dev/null; } # kubectl get, quiet on not found

stop_port_forward() {
  if [[ -n "$PF_PID" ]]; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
  fi
  PF_PID=""
  PF_PORT=""
}

diagnostics() {
  local dir="$ARTIFACTS/$INSTALLER"
  mkdir -p "$dir"
  kubectl get clusters.cluster.x-k8s.io,vclusters.infrastructure.cluster.x-k8s.io -A -o yaml >"$dir/clusters.yaml" 2>&1 || true
  kubectl get helmreleases.helm.toolkit.fluxcd.io,helmrepositories.source.toolkit.fluxcd.io,helmcharts.source.toolkit.fluxcd.io -A -o yaml >"$dir/flux.yaml" 2>&1 || true
  kubectl get pods,pvc,svc,deploy,sts -A -o wide >"$dir/workloads.txt" 2>&1 || true
  kubectl get events -A --sort-by=.lastTimestamp >"$dir/events.txt" 2>&1 || true
  kubectl logs -n capvc-system deploy/capvc-controller-manager --tail=-1 >"$dir/provider.log" 2>&1 || true
  kubectl logs -n capi-system deploy/capi-controller-manager --tail=2000 >"$dir/capi.log" 2>&1 || true
  if [[ "$INSTALLER" == flux ]]; then
    kubectl logs -n flux-system deploy/helm-controller --tail=2000 >"$dir/helm-controller.log" 2>&1 || true
    kubectl logs -n flux-system deploy/source-controller --tail=2000 >"$dir/source-controller.log" 2>&1 || true
  fi
  if [[ -n "$CURRENT_NS" ]]; then
    kubectl logs -n "$CURRENT_NS" -l app=vcluster --all-containers --tail=500 --prefix >"$dir/vcluster.log" 2>&1 || true
  fi
  echo "diagnostics: $dir"
}

summary() {
  echo
  echo "=== e2e-kind ($INSTALLER) ==="
  if ((${#RESULTS[@]})); then printf '%s\n' "${RESULTS[@]}"; fi
}

on_exit() {
  local rc=$?
  stop_port_forward
  if ((rc != 0)); then diagnostics; fi
  summary
  if ((rc == 0)) && [[ "${KEEP_CLUSTER:-}" != 1 ]]; then
    kind delete cluster --name "$KIND_CLUSTER" >/dev/null 2>&1 || true
  else
    echo "kind cluster $KIND_CLUSTER kept: KUBECONFIG=$KUBECONFIG"
  fi
  exit "$rc"
}
trap on_exit EXIT

# --- setup --------------------------------------------------------------------------------------

wait_deployments() { # <namespace>
  kubectl -n "$1" wait deployment --all --for=condition=Available --timeout=300s >/dev/null
}

setup_kind() {
  if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
    log "kind cluster $KIND_CLUSTER exists, reusing it"
    kind export kubeconfig --name "$KIND_CLUSTER" --kubeconfig "$KUBECONFIG"
  else
    log "kind cluster $KIND_CLUSTER ($KIND_NODE_IMAGE)"
    kind create cluster --name "$KIND_CLUSTER" --image "$KIND_NODE_IMAGE" --kubeconfig "$KUBECONFIG" --wait 120s
  fi
}

setup_dependencies() {
  log "cert-manager $CERT_MANAGER_VERSION"
  kubectl apply --server-side -f "https://github.com/cert-manager/cert-manager/releases/download/$CERT_MANAGER_VERSION/cert-manager.yaml" >/dev/null
  wait_deployments cert-manager

  log "Cluster API $CAPI_VERSION core"
  # clusterctl's variables with their defaults (${VAR:=default} → default)
  curl -fsSL "https://github.com/kubernetes-sigs/cluster-api/releases/download/$CAPI_VERSION/core-components.yaml" |
    sed -E 's/\$\{[A-Za-z0-9_]+:=([^}]*)\}/\1/g' >"$WORK/capi.yaml"
  # shellcheck disable=SC2016 # a literal ${
  if grep -q '\${' "$WORK/capi.yaml"; then fail "core-components.yaml has a variable without a default"; fi
  # the webhook's certificate comes from cert-manager: retry until its webhook answers
  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    if kubectl apply --server-side -f "$WORK/capi.yaml" >/dev/null; then break; fi
    ((i == 10)) && fail "apply Cluster API core"
    sleep 10
  done
  wait_deployments capi-system

  log "ServiceMonitor CRD ($PROMETHEUS_OPERATOR_VERSION), PriorityClass $PRIORITY_CLASS"
  kubectl apply --server-side -f "https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/$PROMETHEUS_OPERATOR_VERSION/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml" >/dev/null
  kubectl apply -f - >/dev/null <<EOF
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: $PRIORITY_CLASS
value: 1000000
globalDefault: false
description: Turbacharger's tenant control planes (e2e copy)
EOF

  if [[ "$INSTALLER" == flux ]]; then
    log "Flux source-controller $SOURCE_CONTROLLER_VERSION, helm-controller $HELM_CONTROLLER_VERSION"
    kubectl create namespace flux-system --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    local base
    for base in "fluxcd/source-controller/releases/download/$SOURCE_CONTROLLER_VERSION/source-controller" \
      "fluxcd/helm-controller/releases/download/$HELM_CONTROLLER_VERSION/helm-controller"; do
      kubectl apply --server-side -f "https://github.com/$base.crds.yaml" >/dev/null
      kubectl apply --server-side -n flux-system -f "https://github.com/$base.deployment.yaml" >/dev/null
    done
    # both run as flux-system's default ServiceAccount (flux2's install binds cluster-admin too)
    kubectl create clusterrolebinding flux-e2e --clusterrole=cluster-admin \
      --serviceaccount=flux-system:default --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    wait_deployments flux-system
  fi
}

setup_provider() {
  if [[ "${SKIP_BUILD:-}" != 1 ]]; then
    log "provider image $PROVIDER_IMAGE ($CONTAINER_ENGINE)"
    "$CONTAINER_ENGINE" build -t "$PROVIDER_IMAGE" "$ROOT"
    "$CONTAINER_ENGINE" save -o "$WORK/provider.tar" "$PROVIDER_IMAGE"
    kind load image-archive "$WORK/provider.tar" --name "$KIND_CLUSTER"
    rm -f "$WORK/provider.tar"
  fi
  log "provider (config/default via test/e2e/kind/provider)"
  kubectl kustomize "$ROOT/test/e2e/kind/provider" | kubectl apply --server-side -f - >/dev/null
  kubectl -n capvc-system rollout restart deployment/capvc-controller-manager >/dev/null
  kubectl -n capvc-system rollout status deployment/capvc-controller-manager --timeout=180s >/dev/null
}

# --- vCluster helpers ---------------------------------------------------------------------------

values_for() { # <name>: test/e2e/kind/vcluster.yaml for this vCluster
  sed "s/__TENANT__/$1/g" "$ROOT/test/e2e/kind/vcluster.yaml"
}

indent() { sed "s/^/$(printf '%*s' "$1" '')/"; }

# post_renderers <name> <indent>: Turbacharger's two Flux patches
# (turbacharger-controller/internal/tenant/flux.go, vclusterRelease)
post_renderers() {
  indent "$2" <<EOF
- kustomize:
    patches:
      - target:
          kind: Deployment
          name: $1
        patch: '[{"op": "add", "path": "/spec/template/spec/priorityClassName", "value": "$PRIORITY_CLASS"}]'
      - target:
          group: monitoring.coreos.com
          kind: ServiceMonitor
          name: vc-$1
        patch: |
          apiVersion: monitoring.coreos.com/v1
          kind: ServiceMonitor
          metadata:
            name: vc-$1
            annotations:
              $MONITOR_TLS_ANNOTATION: "true"
EOF
}

prepare_namespace() { # <name>
  CURRENT_NS="vcluster-$1"
  kubectl create namespace "$CURRENT_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  # sync.fromHost.nodes selects turba-env.dev/tenant=<name>, as NAP labels a tenant's nodes
  kubectl label nodes --all "turba-env.dev/tenant=$1" --overwrite >/dev/null
}

apply_cluster() { # <name>
  local name="$1" ns="vcluster-$1"
  {
    cat <<EOF
apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: $name
  namespace: $ns
spec:
  infrastructureRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: VCluster
    name: $name
  controlPlaneRef:
    apiGroup: infrastructure.cluster.x-k8s.io
    kind: VCluster
    name: $name
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: VCluster
metadata:
  name: $name
  namespace: $ns
spec:
  helmRelease:
    installer: $INSTALLER
    chart:
      name: vcluster
      repo: $VCLUSTER_CHART_REPO
      version: $VCLUSTER_CHART_VERSION
    values: |
EOF
    values_for "$name" | indent 6
    if [[ "$INSTALLER" == flux ]]; then
      echo "    postRenderers:"
      post_renderers "$name" 6
    fi
  } >"$WORK/$name.yaml"
  kubectl apply -f "$WORK/$name.yaml" >/dev/null
}

vcluster_provisioned() { [[ "$(jp vcluster.infrastructure.cluster.x-k8s.io -n "vcluster-$1" "$1" -o jsonpath='{.status.initialization.provisioned}')" == true ]]; }
cluster_condition() { # <name> <type> <status>
  [[ "$(jp cluster.cluster.x-k8s.io -n "vcluster-$1" "$1" -o jsonpath="{.status.conditions[?(@.type==\"$2\")].status}")" == "$3" ]]
}
vcluster_condition() { # <name> <type> <status>
  [[ "$(jp vcluster.infrastructure.cluster.x-k8s.io -n "vcluster-$1" "$1" -o jsonpath="{.status.conditions[?(@.type==\"$2\")].status}")" == "$3" ]]
}
exists() { kubectl get "$@" >/dev/null 2>&1; }
gone() { ! exists "$@"; }

# vc_connect <name> <secret> <key>: port-forward to Service <name> and a kubeconfig from Secret
# <secret>, pointed at it (localhost is in the certificate: proxy.extraSANs). Never printed.
vc_connect() {
  local name="$1" secret="$2" key="$3" ns="vcluster-$1"
  stop_port_forward
  (umask 077 && kubectl get secret -n "$ns" "$secret" -o jsonpath="{.data.$key}" | base64 -d >"$WORK/$name.vc.kubeconfig") || return 1
  kubectl port-forward -n "$ns" "svc/$name" :443 >"$WORK/pf.log" 2>&1 &
  PF_PID=$!
  local i
  for i in $(seq 1 40); do
    PF_PORT="$(sed -nE 's/^Forwarding from 127\.0\.0\.1:([0-9]+) .*/\1/p' "$WORK/pf.log" | head -1)"
    [[ -n "$PF_PORT" ]] && return 0
    kill -0 "$PF_PID" 2>/dev/null || break
    sleep 0.5
  done
  stop_port_forward
  return 1
}

vck() { # kubectl inside the connected vCluster
  kubectl --kubeconfig "$WORK/$VC_NAME.vc.kubeconfig" --server "https://localhost:$PF_PORT" --request-timeout=10s "$@"
}

# vc_api_answers <name> <secret> <key>: reconnects (the old forward dies with its pod) and lists
# the vCluster's namespaces
vc_api_answers() {
  VC_NAME="$1"
  vc_connect "$1" "$2" "$3" >/dev/null 2>&1 || return 1
  vck get ns default
}

pod_identity() { # <namespace> <workload label selector>: "name uid startTime restarts" per pod
  kubectl get pods -n "$1" -l "$2" -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.uid} {.status.startTime} {.status.containerStatuses[*].restartCount}{"\n"}{end}' | sort
}

replicas() { jp "$1" -n "$2" "$3" -o jsonpath='{.spec.replicas}'; }
ready_replicas() { jp "$1" -n "$2" "$3" -o jsonpath='{.status.readyReplicas}'; }
ready_is() { [[ "$(ready_replicas "$1" "$2" "$3")" == "$4" ]]; }
no_release() { [[ -z "$(release_secrets "$1")" ]]; }
no_pvcs() { [[ -z "$(kubectl get pvc -n "$1" -o name 2>/dev/null)" ]]; }
nothing_left() { # everything but the namespace's own ConfigMap and ServiceAccount
  ! kubectl get secrets,configmaps,serviceaccounts,services,roles,rolebindings,servicemonitors -n "$1" -o name 2>/dev/null |
    grep -qv -e '^configmap/kube-root-ca.crt$' -e '^serviceaccount/default$'
}
no_pods() { [[ -z "$(kubectl get pods -n "$1" -l "$2" -o name 2>/dev/null)" ]]; }

hr_suspended() { [[ "$(jp helmrelease.helm.toolkit.fluxcd.io -n "vcluster-$1" "$1" -o jsonpath='{.spec.suspend}')" == "$2" ]]; }
hr_ready() {
  local ns="vcluster-$1"
  [[ "$(jp helmrelease.helm.toolkit.fluxcd.io -n "$ns" "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]] &&
    [[ "$(jp helmrelease.helm.toolkit.fluxcd.io -n "$ns" "$1" -o jsonpath='{.status.observedGeneration}')" == "$(jp helmrelease.helm.toolkit.fluxcd.io -n "$ns" "$1" -o jsonpath='{.metadata.generation}')" ]]
}
release_secrets() { kubectl get secrets -n "vcluster-$1" -l "owner=helm,name=$1" -o name 2>/dev/null; }
release_revision() { kubectl get secrets -n "vcluster-$1" -l "owner=helm,name=$1,status=deployed" -o jsonpath='{.items[*].metadata.labels.version}' 2>/dev/null; }

# --- scenarios ----------------------------------------------------------------------------------

scenario_create() { # <name>
  local name="$1" ns="vcluster-$1"
  log "1. create: Cluster + VCluster $ns/$name ($INSTALLER, chart $VCLUSTER_CHART_VERSION)"
  prepare_namespace "$name"
  apply_cluster "$name"
  wait_until 600 "VCluster status.initialization.provisioned" vcluster_provisioned "$name"
  wait_until 120 "VCluster Ready condition True" vcluster_condition "$name" Ready True
  wait_until 60 "Secret $name-kubeconfig exists" exists secret -n "$ns" "$name-kubeconfig"
  wait_until 300 "Cluster InfrastructureReady True" cluster_condition "$name" InfrastructureReady True
  wait_until 300 "Cluster ControlPlaneInitialized True" cluster_condition "$name" ControlPlaneInitialized True
  wait_until 300 "Cluster Available True" cluster_condition "$name" Available True
  kubectl get cluster.cluster.x-k8s.io -n "$ns" "$name" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}' >"$ARTIFACTS/$INSTALLER-cluster-conditions.txt" || true

  wait_until 120 "kubectl get ns in the vCluster through Secret $name-kubeconfig" vc_api_answers "$name" "$name-kubeconfig" value

  if [[ "$INSTALLER" == flux ]]; then
    wait_until 60 "HelmRelease $name Ready" hr_ready "$name"
    [[ "$(kubectl get deploy -n "$ns" "$name" -o jsonpath='{.spec.template.spec.priorityClassName}')" == "$PRIORITY_CLASS" ]] ||
      fail "post-render: Deployment $name has no priorityClassName $PRIORITY_CLASS"
    pass "post-render: Deployment $name priorityClassName $PRIORITY_CLASS"
    [[ "$(kubectl get servicemonitor -n "$ns" "vc-$name" -o jsonpath="{.metadata.annotations.turba-env\.dev/monitor-tls}")" == true ]] ||
      fail "post-render: ServiceMonitor vc-$name has no $MONITOR_TLS_ANNOTATION"
    pass "post-render: ServiceMonitor vc-$name annotation $MONITOR_TLS_ANNOTATION"
  else
    [[ -n "$(release_secrets "$name")" ]] || fail "no helm release Secret for $name"
    pass "helm release $name installed (revision $(release_revision "$name"))"
  fi
}

scenario_park_wake() { # <name>
  local name="$1" ns="vcluster-$1" token
  token="e2e-$RANDOM-$RANDOM"
  log "2. park and wake $ns/$name"
  VC_NAME="$name"
  vck create configmap e2e-data -n default --from-literal=token="$token" >/dev/null
  pass "ConfigMap default/e2e-data written in the vCluster"
  stop_port_forward

  local cp_replicas etcd_replicas
  cp_replicas="$(replicas deploy "$ns" "$name")"
  etcd_replicas="$(replicas sts "$ns" "$name-etcd")"

  kubectl patch cluster.cluster.x-k8s.io -n "$ns" "$name" --type merge -p '{"spec":{"paused":true}}' >/dev/null
  # no Cluster watch either: the pause shows at the next requeue (RequeueAfter 1 min)
  wait_until 120 "VCluster Paused condition True" vcluster_condition "$name" Paused True
  if [[ "$INSTALLER" == flux ]]; then
    wait_until 120 "HelmRelease $name suspended" hr_suspended "$name" true
  fi

  kubectl scale -n "$ns" "deploy/$name" --replicas=0 >/dev/null
  kubectl scale -n "$ns" "sts/$name-etcd" --replicas=0 >/dev/null
  wait_until 180 "control plane pods gone" no_pods "$ns" "app=vcluster,release=$name"
  wait_until 180 "etcd pods gone" no_pods "$ns" "app=vcluster-etcd,release=$name"

  if [[ "$INSTALLER" == flux ]]; then
    # a reconcile request must not wake it either
    kubectl annotate helmrelease.helm.toolkit.fluxcd.io -n "$ns" "$name" --overwrite \
      "reconcile.fluxcd.io/requestedAt=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >/dev/null
  fi
  log "holding parked for ${PARK_SECONDS}s"
  sleep "$PARK_SECONDS"
  [[ "$(replicas deploy "$ns" "$name")" == 0 ]] || fail "Deployment $name scaled back while parked"
  [[ "$(replicas sts "$ns" "$name-etcd")" == 0 ]] || fail "StatefulSet $name-etcd scaled back while parked"
  no_pods "$ns" "app=vcluster,release=$name" || fail "control plane pods came back while parked"
  no_pods "$ns" "app=vcluster-etcd,release=$name" || fail "etcd pods came back while parked"
  pass "still at zero after ${PARK_SECONDS}s"
  if [[ "$INSTALLER" == flux ]]; then
    hr_suspended "$name" true || fail "HelmRelease $name unsuspended while parked"
    pass "HelmRelease $name still suspended"
  fi

  kubectl patch cluster.cluster.x-k8s.io -n "$ns" "$name" --type merge -p '{"spec":{"paused":false}}' >/dev/null
  kubectl scale -n "$ns" "sts/$name-etcd" --replicas="${etcd_replicas:-1}" >/dev/null
  kubectl scale -n "$ns" "deploy/$name" --replicas="${cp_replicas:-1}" >/dev/null
  # the provider watches the Cluster's pause (controllers: SetupWithManager)
  wait_until 60 "VCluster Paused condition False after the Cluster's unpause" vcluster_condition "$name" Paused False
  if [[ "$INSTALLER" == flux ]]; then
    wait_until 60 "HelmRelease $name resumed" hr_suspended "$name" false
  fi
  wait_until 300 "control plane Ready again" ready_is deploy "$ns" "$name" "${cp_replicas:-1}"
  wait_until 180 "vCluster API answers after wake" vc_api_answers "$name" "$name-kubeconfig" value
  local got
  got="$(vck get configmap e2e-data -n default -o jsonpath='{.data.token}')"
  [[ "$got" == "$token" ]] || fail "ConfigMap e2e-data after wake: got '$got'"
  pass "ConfigMap e2e-data survived park and wake"
  wait_until 120 "VCluster Ready condition True after wake" vcluster_condition "$name" Ready True
  stop_port_forward
}

scenario_delete() { # <name>
  local name="$1" ns="vcluster-$1"
  log "3. delete Cluster $ns/$name"
  kubectl delete cluster.cluster.x-k8s.io -n "$ns" "$name" --wait=false >/dev/null
  wait_until 300 "VCluster gone" gone vcluster.infrastructure.cluster.x-k8s.io -n "$ns" "$name"
  wait_until 120 "Cluster gone" gone cluster.cluster.x-k8s.io -n "$ns" "$name"
  wait_until 120 "helm release Secrets gone (chart uninstalled)" no_release "$name"
  if [[ "$INSTALLER" == flux ]]; then
    wait_until 60 "HelmRelease gone" gone helmrelease.helm.toolkit.fluxcd.io -n "$ns" "$name"
    wait_until 60 "HelmRepository $name gone" gone helmrepository.source.toolkit.fluxcd.io -n "$ns" "$name"
  fi
  wait_until 120 "Deployment $name gone" gone deploy -n "$ns" "$name"
  wait_until 120 "StatefulSet $name-etcd gone" gone sts -n "$ns" "$name-etcd"
  wait_until 180 "vCluster pods gone" no_pods "$ns" "release=$name"

  # the provider deletes the chart's PVCs; garbage collection the Secrets it owns
  wait_until 120 "no PVC left in $ns" no_pvcs "$ns"
  wait_until 120 "nothing else left in $ns" nothing_left "$ns"

  kubectl delete namespace "$ns" --wait=false >/dev/null
  wait_until 180 "namespace $ns deleted" gone namespace "$ns"
  CURRENT_NS=""
}

scenario_adopt() { # <name>
  local name="$1" ns="vcluster-$1" token
  token="adopt-$RANDOM-$RANDOM"
  log "4. adopt: a hand-written HelmRelease $ns/$name, then a VCluster of the same name"
  prepare_namespace "$name"
  # as Turbacharger's controller writes it today (turbacharger-controller/internal/tenant/flux.go):
  # HelmRepository loft, HelmRelease <name> with the values and the two post-render patches
  {
    cat <<EOF
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: loft
  namespace: $ns
spec:
  url: $VCLUSTER_CHART_REPO
  interval: 1h
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: $name
  namespace: $ns
spec:
  releaseName: $name
  interval: 10m
  timeout: 10m
  suspend: false
  chart:
    spec:
      chart: vcluster
      version: $VCLUSTER_CHART_VERSION
      sourceRef:
        kind: HelmRepository
        name: loft
  install:
    remediation:
      retries: 3
  upgrade:
    remediation:
      retries: 3
  postRenderers:
EOF
    post_renderers "$name" 4
    echo "  values:"
    values_for "$name" | indent 4
  } >"$WORK/$name-hr.yaml"
  kubectl apply -f "$WORK/$name-hr.yaml" >/dev/null
  wait_until 600 "hand-written HelmRelease $name Ready" hr_ready "$name"
  wait_until 300 "control plane Ready" ready_is deploy "$ns" "$name" 1
  wait_until 120 "vCluster API answers through Secret vc-$name" vc_api_answers "$name" "vc-$name" config
  vck create configmap e2e-data -n default --from-literal=token="$token" >/dev/null
  pass "ConfigMap default/e2e-data written in the vCluster"
  stop_port_forward

  local before_cp before_etcd before_rev
  before_cp="$(pod_identity "$ns" "app=vcluster,release=$name")"
  before_etcd="$(pod_identity "$ns" "app=vcluster-etcd,release=$name")"
  before_rev="$(release_revision "$name")"
  printf 'before:\n%s\n%s\nrevision %s\n' "$before_cp" "$before_etcd" "$before_rev" >"$ARTIFACTS/adopt-pods.txt"

  # Turbacharger's hand-over: suspend its HelmRelease, then the VCluster takes it
  kubectl patch helmrelease.helm.toolkit.fluxcd.io -n "$ns" "$name" --type merge -p '{"spec":{"suspend":true}}' >/dev/null
  apply_cluster "$name"

  # EXPECTED FAILURE (marked, not hidden): if the provider can't take the HelmRelease over (its
  # owner reference, a conflicting field), the steps below fail; set EXPECT_ADOPT_FAILURE=1 to
  # record it as a note and go on. The provider's flux installer (controllers/flux.go,
  # fluxDeploy) sets itself as controller owner of an existing HelmRelease without one and
  # points it at its own HelmRepository <name>.
  if ! adopt_checks "$name" "$token" "$before_cp" "$before_etcd" "$before_rev"; then
    if [[ "${EXPECT_ADOPT_FAILURE:-}" == 1 ]]; then
      note "adoption failed as expected (EXPECT_ADOPT_FAILURE=1): $ADOPT_ERROR"
    else
      fail "adoption: $ADOPT_ERROR"
    fi
  fi
  scenario_delete "$name"
}

ADOPT_ERROR=""
adopt_check() { # <description> <command...>: like wait_until, but records instead of exiting
  local what="$1"
  shift
  if "$@"; then
    pass "$what"
  else
    ADOPT_ERROR="$what"
    return 1
  fi
}
poll() { # <seconds> <command...>
  local deadline=$((SECONDS + $1))
  shift
  until "$@" >/dev/null 2>&1; do
    ((SECONDS >= deadline)) && return 1
    sleep 3
  done
}

adopt_checks() {
  local name="$1" token="$2" before_cp="$3" before_etcd="$4" before_rev="$5" ns="vcluster-$1"
  adopt_check "adopted: VCluster provisioned" poll 600 vcluster_provisioned "$name" || return 1
  adopt_check "adopted: Secret $name-kubeconfig exists" poll 60 exists secret -n "$ns" "$name-kubeconfig" || return 1
  adopt_check "adopted: HelmRelease resumed" poll 60 hr_suspended "$name" false || return 1
  adopt_check "adopted: HelmRelease Ready at its new generation" poll 300 hr_ready "$name" || return 1
  local owner
  owner="$(jp helmrelease.helm.toolkit.fluxcd.io -n "$ns" "$name" -o jsonpath='{.metadata.ownerReferences[?(@.controller==true)].kind}/{.metadata.ownerReferences[?(@.controller==true)].name}')"
  adopt_check "adopted: HelmRelease controller owner is VCluster/$name (got $owner)" test "$owner" == "VCluster/$name" || return 1
  # let a helm upgrade or a drift correction show, if there is one
  sleep 30
  local after_cp after_etcd after_rev
  after_cp="$(pod_identity "$ns" "app=vcluster,release=$name")"
  after_etcd="$(pod_identity "$ns" "app=vcluster-etcd,release=$name")"
  after_rev="$(release_revision "$name")"
  printf 'after:\n%s\n%s\nrevision %s\n' "$after_cp" "$after_etcd" "$after_rev" >>"$ARTIFACTS/adopt-pods.txt"
  if [[ "$after_rev" != "$before_rev" ]]; then
    note "adoption upgraded the helm release: revision $before_rev → $after_rev"
  else
    pass "adoption left the helm release at revision $before_rev"
  fi
  adopt_check "adopted: control plane pod not restarted (same UID, start time, restarts)" test "$after_cp" == "$before_cp" || return 1
  adopt_check "adopted: etcd pod not restarted" test "$after_etcd" == "$before_etcd" || return 1
  adopt_check "adopted: API answers through Secret $name-kubeconfig" poll 120 vc_api_answers "$name" "$name-kubeconfig" value || return 1
  local got
  got="$(vck get configmap e2e-data -n default -o jsonpath='{.data.token}' 2>/dev/null || true)"
  stop_port_forward
  adopt_check "adopted: ConfigMap e2e-data survived" test "$got" == "$token" || return 1
}

# --- main ---------------------------------------------------------------------------------------

log "e2e-kind: installer $INSTALLER, work dir $WORK"
setup_kind
setup_dependencies
setup_provider

NAME="e2e-$INSTALLER"
scenario_create "$NAME"
scenario_park_wake "$NAME"
scenario_delete "$NAME"
if [[ "$INSTALLER" == flux ]]; then
  scenario_adopt adopt
fi
log "e2e-kind ($INSTALLER) passed"
