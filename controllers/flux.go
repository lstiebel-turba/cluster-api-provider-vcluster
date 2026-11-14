package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"
)

var (
	helmReleaseGVK    = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}
	helmRepositoryGVK = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "HelmRepository"}
)

// The flux installer writes a HelmRepository and a HelmRelease per VCluster (uncached, unstructured).
//+kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;create;patch;delete
//+kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmrepositories,verbs=get;create;patch

// FluxOptions configures the flux installer.
type FluxOptions struct {
	// RepositoryName names a HelmRepository shared by the VClusters of a namespace, never deleted
	// by the provider. Empty: one per VCluster, named after it and owned by it.
	RepositoryName string

	// DriftDetection is the HelmRelease's spec.driftDetection.mode: enabled, warn or disabled.
	DriftDetection string
}

// chartSpec is what either installer installs.
type chartSpec struct {
	repo, name, version string
	values              string // YAML or JSON
	postRenderers       []any
}

// fluxState is the HelmRelease as the flux installer reads it.
type fluxState struct {
	ready   bool   // Ready and current
	failed  bool   // Stalled: Flux gave up
	message string // why not ready
}

func newUnstructured(gvk schema.GroupVersionKind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

func (r *GenericReconciler) fluxRepositoryName(vCluster GenericVCluster) string {
	if r.Options.Flux.RepositoryName != "" {
		return r.Options.Flux.RepositoryName
	}
	return vCluster.GetName()
}

// fluxDeploy writes the HelmRepository and the HelmRelease and reads the HelmRelease back.
func (r *GenericReconciler) fluxDeploy(ctx context.Context, vCluster GenericVCluster, chart chartSpec) (fluxState, error) {
	values := map[string]any{}
	if chart.values != "" {
		if err := yaml.Unmarshal([]byte(chart.values), &values); err != nil {
			return fluxState{}, fmt.Errorf("parse values: %w", err)
		}
	}

	repoName := r.fluxRepositoryName(vCluster)
	repo := newUnstructured(helmRepositoryGVK, vCluster.GetNamespace(), repoName)
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, repo, func() error {
		spec := map[string]any{"url": chart.repo, "interval": "1h"}
		if strings.HasPrefix(chart.repo, "oci://") {
			spec["type"] = "oci"
		}
		repo.Object["spec"] = spec
		if r.Options.Flux.RepositoryName != "" {
			return nil
		}
		return controllerutil.SetControllerReference(vCluster, repo, r.Client.Scheme())
	})
	if err != nil {
		return fluxState{}, fmt.Errorf("HelmRepository %s: %w", repoName, err)
	}

	release := newUnstructured(helmReleaseGVK, vCluster.GetNamespace(), vCluster.GetName())
	_, err = controllerutil.CreateOrPatch(ctx, r.Client, release, func() error {
		chartTemplate := map[string]any{
			"chart":     chart.name,
			"sourceRef": map[string]any{"kind": "HelmRepository", "name": repoName},
			"interval":  "10m",
		}
		if chart.version != "" {
			chartTemplate["version"] = chart.version
		}
		spec := map[string]any{
			"releaseName": vCluster.GetName(),
			"interval":    "10m",
			"timeout":     "10m",
			"chart":       map[string]any{"spec": chartTemplate},
			"install":     map[string]any{"remediation": map[string]any{"retries": int64(3)}},
			"upgrade":     map[string]any{"remediation": map[string]any{"retries": int64(3), "remediateLastFailure": true}},
			"values":      values,
			"suspend":     false,
		}
		if mode := r.Options.Flux.DriftDetection; mode != "" {
			spec["driftDetection"] = map[string]any{"mode": mode}
		}
		if len(chart.postRenderers) > 0 {
			spec["postRenderers"] = chart.postRenderers
		}
		release.Object["spec"] = spec
		return controllerutil.SetControllerReference(vCluster, release, r.Client.Scheme())
	})
	if err != nil {
		return fluxState{}, fmt.Errorf("HelmRelease %s: %w", vCluster.GetName(), err)
	}
	// read the status back: CreateOrPatch may leave the one from before the patch
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(release), release); err != nil {
		return fluxState{}, fmt.Errorf("HelmRelease %s: %w", vCluster.GetName(), err)
	}
	return readHelmRelease(release), nil
}

func readHelmRelease(release *unstructured.Unstructured) fluxState {
	observed, _, _ := unstructured.NestedInt64(release.Object, "status", "observedGeneration")
	conditions, _, _ := unstructured.NestedSlice(release.Object, "status", "conditions")
	state := fluxState{message: "waiting for helm-controller"}
	for _, c := range conditions {
		condition, ok := c.(map[string]any)
		if !ok {
			continue
		}
		status, _ := condition["status"].(string)
		message, _ := condition["message"].(string)
		switch condition["type"] {
		case "Stalled":
			if status == string(metav1.ConditionTrue) {
				state.failed, state.message = true, message
				return state
			}
		case "Ready":
			if status == string(metav1.ConditionTrue) && observed == release.GetGeneration() {
				state.ready, state.message = true, ""
			} else if message != "" {
				state.message = message
			}
		}
	}
	return state
}

// fluxSuspend sets the HelmRelease's spec.suspend, while the Cluster or the VCluster is paused.
func (r *GenericReconciler) fluxSuspend(ctx context.Context, vCluster GenericVCluster, suspend bool) error {
	release := newUnstructured(helmReleaseGVK, vCluster.GetNamespace(), vCluster.GetName())
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: vCluster.GetNamespace(), Name: vCluster.GetName()}, release)
	if kerrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if current, _, _ := unstructured.NestedBool(release.Object, "spec", "suspend"); current == suspend {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"suspend": suspend}})
	if err != nil {
		return err
	}
	return r.Client.Patch(ctx, release, client.RawPatch(types.MergePatchType, patch))
}

// fluxRemove deletes the HelmRelease, Flux uninstalls the chart; gone once it no longer exists.
// A per-VCluster HelmRepository goes with the VCluster (owner reference).
func (r *GenericReconciler) fluxRemove(ctx context.Context, vCluster GenericVCluster) (bool, error) {
	release := newUnstructured(helmReleaseGVK, vCluster.GetNamespace(), vCluster.GetName())
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: vCluster.GetNamespace(), Name: vCluster.GetName()}, release)
	if kerrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if release.GetDeletionTimestamp() == nil {
		// helm-controller skips the uninstall of a suspended HelmRelease
		if err := r.fluxSuspend(ctx, vCluster, false); err != nil {
			return false, err
		}
		if err := r.Client.Delete(ctx, release); err != nil && !kerrors.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}
