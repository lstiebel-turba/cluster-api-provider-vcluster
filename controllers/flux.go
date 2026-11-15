package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
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

// fluxReconcileAnnotations are Flux's reconcile requests: set on a VCluster, they go on to its
// HelmRelease (requestedAt also on its HelmRepository), so whoever owns the VCluster asks
// helm-controller for a reconcile, a reset of the retries or a forced upgrade.
var fluxReconcileAnnotations = []string{
	"reconcile.fluxcd.io/requestedAt",
	"reconcile.fluxcd.io/resetAt",
	"reconcile.fluxcd.io/forceAt",
}

var (
	helmReleaseGVK    = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}
	helmRepositoryGVK = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "HelmRepository"}
)

// The flux installer writes a HelmRepository and a HelmRelease per VCluster (uncached, unstructured).
// The HelmReleases' metadata is watched (cached): a change reconciles the VCluster that controls one.
//+kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch;create;patch;delete
//+kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmrepositories,verbs=get;create;patch

// FluxOptions configures the flux installer.
type FluxOptions struct {
	// RepositoryName names a HelmRepository shared by the VClusters of a namespace, never deleted
	// by the provider. Empty: one per VCluster, named after it and owned by it.
	RepositoryName string

	// RepositoryProvider is an OCI HelmRepository's spec.provider (generic, aws, azure, gcp):
	// source-controller logs in to the registry with its cloud identity, for example to a mirror
	// in Artifact Registry. Empty: none (generic).
	RepositoryProvider string

	// DriftDetection is the HelmRelease's spec.driftDetection.mode: enabled, warn or disabled.
	DriftDetection string

	// ServiceAccountName is the HelmRelease's spec.serviceAccountName: helm-controller
	// impersonates this ServiceAccount of the VCluster's namespace to install the chart, so the
	// chart gets that account's rights instead of helm-controller's. Empty: none (helm-controller's
	// own rights, or its --default-service-account).
	ServiceAccountName string
}

// chartSpec is what either installer installs.
type chartSpec struct {
	repo, name, version string
	values              string // YAML or JSON
	postRenderers       []any
	driftDetection      string // the HelmRelease's driftDetection.mode, "" the provider's
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

// copiedLabelsAnnotation lists the label keys copyFluxMetadata copied, comma-separated: a label
// gone from the VCluster goes from the Flux object too.
const copiedLabelsAnnotation = "vcluster.loft.sh/copied-labels"

// copyFluxMetadata puts the VCluster's labels and the given Flux annotations it carries on
// obj, so selectors and watches on the VCluster's labels find the Flux objects too. A label it
// copied before and the VCluster no longer has is removed; other labels and annotations of obj's
// own stay.
func copyFluxMetadata(vCluster GenericVCluster, obj *unstructured.Unstructured, annotationKeys []string) {
	labels, annotations := obj.GetLabels(), obj.GetAnnotations()
	if labels == nil {
		labels = map[string]string{}
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	vcLabels := vCluster.GetLabels()
	for _, key := range strings.Split(annotations[copiedLabelsAnnotation], ",") {
		if _, ok := vcLabels[key]; key != "" && !ok {
			delete(labels, key)
		}
	}
	maps.Copy(labels, vcLabels)
	if len(vcLabels) > 0 {
		annotations[copiedLabelsAnnotation] = strings.Join(slices.Sorted(maps.Keys(vcLabels)), ",")
	} else {
		delete(annotations, copiedLabelsAnnotation)
	}
	for _, key := range annotationKeys {
		if value, ok := vCluster.GetAnnotations()[key]; ok {
			annotations[key] = value
		}
	}
	if len(labels) > 0 || obj.GetLabels() != nil {
		obj.SetLabels(labels)
	}
	if len(annotations) > 0 || obj.GetAnnotations() != nil {
		obj.SetAnnotations(annotations)
	}
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
		// field by field: what someone else set on a shared repository (a secretRef) stays
		spec, _, _ := unstructured.NestedMap(repo.Object, "spec")
		if spec == nil {
			spec = map[string]any{}
		}
		spec["url"], spec["interval"] = chart.repo, "1h"
		delete(spec, "type")
		delete(spec, "provider")
		if strings.HasPrefix(chart.repo, "oci://") {
			spec["type"] = "oci"
			if provider := r.Options.Flux.RepositoryProvider; provider != "" {
				spec["provider"] = provider
			}
		}
		repo.Object["spec"] = spec
		if r.Options.Flux.RepositoryName != "" {
			return nil
		}
		// a VCluster's own repository: its labels and reconcile requests, as its HelmRelease
		copyFluxMetadata(vCluster, repo, fluxReconcileAnnotations[:1])
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
		mode := r.Options.Flux.DriftDetection
		if chart.driftDetection != "" {
			mode = chart.driftDetection
		}
		if mode != "" {
			spec["driftDetection"] = map[string]any{"mode": mode}
		}
		if len(chart.postRenderers) > 0 {
			spec["postRenderers"] = chart.postRenderers
		}
		if sa := r.Options.Flux.ServiceAccountName; sa != "" {
			spec["serviceAccountName"] = sa
		}
		release.Object["spec"] = spec
		copyFluxMetadata(vCluster, release, fluxReconcileAnnotations)
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
// A HelmRelease the VCluster doesn't control (someone else's, an adoption that failed) stays as it is.
func (r *GenericReconciler) fluxSuspend(ctx context.Context, vCluster GenericVCluster, suspend bool) error {
	release := newUnstructured(helmReleaseGVK, vCluster.GetNamespace(), vCluster.GetName())
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: vCluster.GetNamespace(), Name: vCluster.GetName()}, release)
	if kerrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(release, vCluster) {
		return nil
	}
	return r.fluxSetSuspend(ctx, release, suspend)
}

func (r *GenericReconciler) fluxSetSuspend(ctx context.Context, release *unstructured.Unstructured, suspend bool) error {
	if current, _, _ := unstructured.NestedBool(release.Object, "spec", "suspend"); current == suspend {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"suspend": suspend}})
	if err != nil {
		return err
	}
	return r.Client.Patch(ctx, release, client.RawPatch(types.MergePatchType, patch))
}

// releaseDeletedAnnotation marks a VCluster whose HelmRelease the provider itself deleted: only
// then is a missing HelmRelease the provider's uninstall, and the chart's volumes go too.
const releaseDeletedAnnotation = "vcluster.loft.sh/release-deleted"

// fluxRemove deletes the HelmRelease, Flux uninstalls the chart; gone once it no longer exists.
// A per-VCluster HelmRepository goes with the VCluster (owner reference). A HelmRelease of that
// name the VCluster doesn't control is left alone: gone, but not owned, so the chart's volumes
// stay too. So does a HelmRelease that is missing without the provider deleting it (never
// written, or removed by someone else): the chart may still run, and its volumes hold its data.
func (r *GenericReconciler) fluxRemove(ctx context.Context, vCluster GenericVCluster) (gone, owned bool, err error) {
	release := newUnstructured(helmReleaseGVK, vCluster.GetNamespace(), vCluster.GetName())
	err = r.Client.Get(ctx, types.NamespacedName{Namespace: vCluster.GetNamespace(), Name: vCluster.GetName()}, release)
	if kerrors.IsNotFound(err) {
		return true, vCluster.GetAnnotations()[releaseDeletedAnnotation] == "true", nil
	}
	if err != nil {
		return false, false, err
	}
	if !metav1.IsControlledBy(release, vCluster) {
		return true, false, nil
	}
	if release.GetDeletionTimestamp() == nil {
		// helm-controller skips the uninstall of a suspended HelmRelease
		if err := r.fluxSetSuspend(ctx, release, false); err != nil {
			return false, true, err
		}
		if err := r.markReleaseDeleted(ctx, vCluster); err != nil {
			return false, true, err
		}
		if err := r.Client.Delete(ctx, release); err != nil && !kerrors.IsNotFound(err) {
			return false, true, err
		}
	}
	return false, true, nil
}

// markReleaseDeleted sets releaseDeletedAnnotation before the provider deletes the HelmRelease.
func (r *GenericReconciler) markReleaseDeleted(ctx context.Context, vCluster GenericVCluster) error {
	if vCluster.GetAnnotations()[releaseDeletedAnnotation] == "true" {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{releaseDeletedAnnotation: "true"}}})
	if err != nil {
		return err
	}
	return r.Client.Patch(ctx, vCluster, client.RawPatch(types.MergePatchType, patch))
}
