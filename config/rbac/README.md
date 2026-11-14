# RBAC

The provider runs as ServiceAccount `capvc-controller-manager` in `capvc-system`. No binding to
`cluster-admin`; `test/rbac` checks that and the rules below.

| Manifest | Bound | For |
| --- | --- | --- |
| `role.yaml` (`manager-role`) | ClusterRoleBinding | The provider's own API calls, generated from the `+kubebuilder:rbac` markers in `controllers/` by `make manifests`. Enough for the flux installer. |
| `leader_election_role.yaml` | RoleBinding in `capvc-system` | `--leader-elect`: the Lease and its events. |
| `helm-installer/` (component) | ClusterRoleBinding | The helm installer: helm runs with the provider's ServiceAccount. |
| `namespaced/` (component) | ClusterRoleBinding plus your RoleBindings | `manager-role` split into a cluster-wide and a per-namespace part. |

## Overlays

| Overlay | Installer | Rights |
| --- | --- | --- |
| `config/default` (`make release`) | helm (default) | `manager-role` and `helm-installer-role`, cluster-wide. |
| `config/flux-installer` | `--installer=flux` | `manager-role` cluster-wide, no chart rights (helm-controller's, see [Per namespace](#per-namespace-flux-installer)). |
| `config/flux-installer/namespaced` | `--installer=flux` | `manager-cluster-role` cluster-wide, `manager-namespace-role` per namespace. |

## manager-role

| API group | Resource | Verbs | Why |
| --- | --- | --- | --- |
| `infrastructure.cluster.x-k8s.io`, `controlplane.cluster.x-k8s.io` | `vclusters` | get, list, watch, update, patch | The cache; finalizers by update, spec and conditions by patch. |
| same | `vclusters/status` | patch | Status. |
| same | `vclusters/finalizers` | update | Owner references with `blockOwnerDeletion` on the HelmRelease and HelmRepository. |
| `cluster.x-k8s.io` | `clusters` | get, list, watch | The owner Cluster (cached). |
| core | `namespaces` | get | `--namespace-selector`, a namespace being deleted. |
| core | `secrets` | get, create, patch | Reads `vc-<name>` and `<name>-certs`, writes `<cluster>-kubeconfig` and `<cluster>-ca`. No list or watch. |
| core | `services` | get | The control plane host. |
| core | `persistentvolumeclaims` | list, delete | The chart's volumes (labels `app=vcluster` or `app=vcluster-etcd`, `release=<name>`), after the chart is gone. |
| `helm.toolkit.fluxcd.io` | `helmreleases` | get, create, patch, delete | flux installer. |
| `source.toolkit.fluxcd.io` | `helmrepositories` | get, create, patch | flux installer; a per-VCluster one goes with the VCluster by owner reference. |
| `infrastructure.cluster.x-k8s.io` | `kubevirtclusters`, `nutanixclusters`, `openstackclusters`, `packetclusters` (and `/status`) | get, patch | A foreign infrastructure cluster gets the control plane endpoint. |

Whether the Cluster kind exists is read from discovery, which needs no rule.

An infrastructure cluster of another kind (not a VCluster) needs a rule of its own, for example
`get` and `patch` on `fooclusters` (and `patch` on `fooclusters/status` if it has
`status.ready`). Add it to a ClusterRole bound to the provider; the annotation
`vcluster.loft.sh/skip-infrastructure-cluster-patch: "true"` on the VCluster skips the patch.

## Per namespace (flux installer)

`namespaced/` drops `manager-role` and its binding and adds:

- `manager-cluster-role`, bound cluster-wide: VClusters, Clusters, namespaces, infrastructure
  clusters.
- `manager-namespace-role`, not bound: Secrets, Services, PersistentVolumeClaims, HelmReleases,
  HelmRepositories. Bind it in each namespace with VClusters, and limit the provider to them with
  `--namespace` or `--namespace-selector`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: capvc-manager
  namespace: tenant-a
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: capvc-manager-namespace-role
subjects:
- kind: ServiceAccount
  name: capvc-controller-manager
  namespace: capvc-system
```

The provider holds no right on the chart's objects: helm-controller installs the chart. Without
`--flux-service-account` it does so with its own ServiceAccount, by default bound to
`cluster-admin`, so whoever may write a VCluster's spec (`chart.repo`, values, `postRenderers`)
gets helm-controller's rights through the chart. To confine that, create a ServiceAccount with
the chart's rights (the list under [the helm installer](#still-broad-the-helm-installer), as a
Role where it can be) in each namespace with VClusters and start the provider with
`--flux-service-account=<name>`: the HelmRelease's `spec.serviceAccountName`, which helm-controller
impersonates. helm-controller's `--default-service-account` does the same for every HelmRelease
without one.

## Still broad: the helm installer

With the helm installer the provider runs `helm upgrade --install` and `helm delete` itself, so it
needs every right the vCluster chart's objects need: Secrets (helm's release storage too),
Services, ServiceAccounts, StatefulSets, Deployments, Roles, RoleBindings, ClusterRoles,
ClusterRoleBindings and the optional Ingress, NetworkPolicy, PodDisruptionBudget, ServiceMonitor,
TLSRoute, LimitRange and ResourceQuota. The chart's Role and ClusterRole grant rights the provider
does not hold, so `helm-installer-role` carries `escalate` and `bind` on Roles and ClusterRoles:
in practice as strong as `cluster-admin`, cluster-wide. Objects from
`experimental.deploy.host.manifests` or another chart need rules of their own. Where that
matters use the flux installer with `--flux-service-account` (above); the flux installer alone
moves these rights to helm-controller.
