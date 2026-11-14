# The fork

This repository is Turbalance's fork of
[loft-sh/cluster-api-provider-vcluster](https://github.com/loft-sh/cluster-api-provider-vcluster)
(`upstream`). Its changes: the CAPI v1beta2 contract, the `flux` installer, least-privilege RBAC,
the kind and kwok end-to-end tests; the vNode and private-nodes controllers are removed.

**Contents:** [Upstream sync](#upstream-sync) · [Offering a change upstream](#offering-a-change-upstream) · [Licence (MPL-2.0)](#licence-mpl-20) · [Dependency updates](#dependency-updates)

## Upstream sync

[hack/upstream-sync.sh](../hack/upstream-sync.sh) fetches upstream `main` and lists its commits the
fork has neither merged (the commit or a patch-identical copy) nor skipped. It exits 1 when it lists
any. [upstream-watch.yaml](../.github/workflows/upstream-watch.yaml) runs it every Monday: a red run
is the notification (no issues, no comments).

```sh
git remote add upstream https://github.com/loft-sh/cluster-api-provider-vcluster.git   # once
hack/upstream-sync.sh                 # list
hack/upstream-sync.sh skip <sha> <reason...>
```

For each listed commit, one of:

- **Take it:** merge upstream into a branch off the fork's main, `git merge upstream/main`, and open
  a pull request in the fork. A merge, not a rebase: upstream's commits stay ancestors, so the list
  empties. Conflicts are usual in the files the fork rewrote (`controllers/`, `api/`, `config/rbac/`,
  the workflows); keep the fork's behaviour and take upstream's intent. Commits on files the fork
  removed (the vNode controllers, `hack/vnode/`, `pkg/util/conditions`, `pkg/util/patch`) resolve to
  the deletion.
- **Take only some:** `git cherry-pick <sha>`. A clean pick is recognised; one picked with changes
  isn't, so also skip it: `hack/upstream-sync.sh skip <sha> picked as <fork sha>`.
- **Leave it:** `hack/upstream-sync.sh skip <sha> <why>` (for example: vNode only, upstream's
  release or CI). The reason lands in [hack/upstream-skipped.txt](../hack/upstream-skipped.txt),
  committed with the pull request.

Then `make test` and `make e2e-kwok`, and the e2e-kind workflow on the pull request.

## Offering a change upstream

A fork change that would help upstream (a bug fix, the v1beta2 contract) can go back as an upstream
pull request, **only with the human's go-ahead for that one change**. Nothing is sent upstream
automatically: no workflow, script or bot here opens upstream pull requests, issues or comments.

1. Ask the human, naming the change and why upstream would want it.
2. Branch from `upstream/main`, cherry-pick the change, strip what is Turbalance-specific (names,
   labels such as `turba-env.dev/*`, our registries), and keep upstream's style.
3. The human opens the pull request (or says to open it from their account), following upstream's
   contribution rules.
4. Once upstream merges it, the next sync brings it back; skip the fork's original commit's
   upstream twin if the patches differ.

## Licence (MPL-2.0)

[LICENSE](../LICENSE) is the Mozilla Public License 2.0 (upstream added it in `ac795b4f`). What
that asks of the fork:

- **Modified files stay MPL-2.0.** A file of upstream's that the fork changes keeps the MPL; its
  notices stay as they are. New fork files are MPL-2.0 too (the repository's licence).
- **Source for every published image.** MPL 2.0 section 3.2: whoever receives the executable must be
  able to get its source. The repository is public, CI labels each image with
  `org.opencontainers.image.source` and `org.opencontainers.image.revision` (the commit), and the
  [Dockerfile](../Dockerfile) sets `org.opencontainers.image.licenses=MPL-2.0`. A release image
  needs the same labels and its tag pushed; never publish an image built from an unpushed commit.
- **Third-party code keeps its licence.** `vendor/` and `pkg/helm/secrets.go` (copied from Helm,
  Apache-2.0); the image also carries the Helm binary (Apache-2.0).

**Headers that say Apache:** the kubebuilder scaffold put an Apache-2.0 header ("Copyright 2022.")
on `main.go`, `api/controlplane/v1alpha1/{groupversion_info,vcluster_types}.go`,
`api/infrastructure/v1alpha1/{groupversion_info,vcluster_types}.go` and
`controllers/vcluster_controller.go`, while the LICENSE file is MPL-2.0. The fork leaves these
headers alone: choosing the licence of upstream's files is upstream's call. Raising it upstream
follows [Offering a change upstream](#offering-a-change-upstream).

## Dependency updates

[renovate.json](../renovate.json), run by the hosted Renovate app (Mend's GitHub app, installed
on this repository only), opens PRs before 06:00 UTC on Mondays, after a release is 3 days old,
one per group:

| Group | What |
| --- | --- |
| kubernetes and cluster-api | `k8s.io/*` and `sigs.k8s.io/*` modules, `CAPI_VERSION` in `hack/e2e-kwok.env` and `hack/e2e-kind.sh`, `KWOK_KUBE_VERSION` |
| go modules | the other Go modules |
| go toolchain | `go.mod`'s `toolchain` and the Dockerfile's `golang` tag, patches only |
| github actions | `uses:` SHAs and their version comments |
| flux and vcluster (follow the host) | the e2e Flux controllers and vCluster chart: merge once the host runs the same release |
| tools and images | the rest: kind, its node image, cert-manager, kwok, Helm, the prometheus-operator CRD, govulncheck, setup-envtest's commit, base image digests |

- **Bounds instead of approvals:** without a dependency dashboard (no issues) a minor can't wait for
  approval, so it is held by `allowedVersions`. Cluster API stays below 1.14 (the host's
  `CAPI_VERSION`), controller-runtime below 0.24 and `k8s.io` below 0.36 (what CAPI 1.13 builds on),
  kind's node image on 1.35, Helm on 3, Go on 1.26. Moving to the next CAPI minor: raise the three
  bounds together, `currentValue=release-0.23` of setup-envtest in `ci.yaml`, envtest's
  `ENVTEST_K8S_VERSION` and the node image.
- **Vendoring:** Renovate runs `go mod tidy` and, because `vendor/modules.txt` exists, `go mod
  vendor` itself, also on the hosted app (`postUpgradeTasks` would need a self-hosted Renovate).
- **Checksums:** a version with a sha256 pin next to it (Helm in the Dockerfile, kind in
  `e2e-kind.yaml`, kwokctl, the Flux controllers and CAPI in `hack/e2e-kwok.env`) fails CI until
  `hack/update-checksums.sh` is run on the PR's branch and the result committed.
- **A new pin** needs a `# renovate: datasource=… depName=…` line directly above it (the custom
  manager in `renovate.json` names the files it reads), or it is never updated.
- Validate edits: `npx --package renovate@44 -- renovate-config-validator --strict renovate.json`.
