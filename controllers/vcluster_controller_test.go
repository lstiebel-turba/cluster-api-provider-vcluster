package controllers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1alpha1 "github.com/loft-sh/cluster-api-provider-vcluster/api/infrastructure/v1alpha1"
	"github.com/loft-sh/cluster-api-provider-vcluster/pkg/helm"
)

type countingHelm struct {
	upgrades int
	fail     error // Upgrade's error
}

func (c *countingHelm) Install(_, _ string, _ helm.UpgradeOptions) error { return nil }
func (c *countingHelm) Upgrade(_, _ string, _ helm.UpgradeOptions) error {
	c.upgrades++
	return c.fail
}
func (c *countingHelm) Rollback(_, _ string, _ string) error { return nil }
func (c *countingHelm) Delete(_, _ string) error             { return nil }
func (c *countingHelm) Exists(_, _ string) (bool, error)     { return false, nil }

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, clusterv1.AddToScheme, infrastructurev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

// ownedVCluster is owned by Cluster "c1" the way Cluster API v1.13.11+ sets it: no controller=true.
func ownedVCluster() *infrastructurev1alpha1.VCluster {
	return &infrastructurev1alpha1.VCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "vc1",
			Namespace:  "ns",
			Finalizers: []string{CleanupFinalizer},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "Cluster",
				Name:       "c1",
				UID:        "uid",
			}},
		},
	}
}

func reconcileOnce(t *testing.T, objs ...client.Object) (*countingHelm, *infrastructurev1alpha1.VCluster) {
	t.Helper()
	return reconcileWith(t, Options{}, objs...)
}

func reconcileWith(t *testing.T, options Options, objs ...client.Object) (*countingHelm, *infrastructurev1alpha1.VCluster) {
	t.Helper()
	h := &countingHelm{}
	c := fakeclient.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns", Labels: map[string]string{"tenant": "yes"}}})...).
		WithStatusSubresource(&infrastructurev1alpha1.VCluster{}).
		Build()
	r := &GenericReconciler{
		Client:            c,
		HelmClient:        h,
		Log:               logr.Discard(),
		Scheme:            c.Scheme(),
		Options:           options,
		clusterKindExists: true,
		HTTPClientGetter:  NewHTTPClientGetter(),
		New:               func() GenericVCluster { return &infrastructurev1alpha1.VCluster{} },
	}
	// twice: the first pass only sets the Paused condition and requeues
	var errs []error
	for range 2 {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "vc1"}})
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		t.Errorf("reconcile: %v", errs)
	}
	got := &infrastructurev1alpha1.VCluster{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, got); err != nil {
		t.Fatal(err)
	}
	return h, got
}

func TestPausedClusterSkipsHelm(t *testing.T) {
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"},
		Spec:       clusterv1.ClusterSpec{Paused: ptr.To(true)},
	}
	h, got := reconcileOnce(t, cluster, ownedVCluster())
	if h.upgrades != 0 {
		t.Errorf("helm upgrades while paused: %d", h.upgrades)
	}
	if !conditions.IsTrue(got, infrastructurev1alpha1.PausedCondition) {
		t.Errorf("Paused condition not true: %+v", got.Status.Conditions)
	}
}

func TestOwnerWithoutControllerFlagDeploys(t *testing.T) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}
	h, got := reconcileOnce(t, cluster, ownedVCluster())
	if h.upgrades != 1 {
		t.Errorf("helm upgrades: %d, want 1", h.upgrades)
	}
	if !conditions.IsTrue(got, infrastructurev1alpha1.HelmChartDeployedCondition) {
		t.Errorf("HelmChartDeployed not true: %+v", got.Status.Conditions)
	}
	if !conditions.IsFalse(got, infrastructurev1alpha1.PausedCondition) {
		t.Errorf("Paused condition not false: %+v", got.Status.Conditions)
	}
	// no kubeconfig yet: both summaries false, nothing initialized
	for _, conditionType := range []string{infrastructurev1alpha1.ReadyCondition, infrastructurev1alpha1.AvailableCondition} {
		if !conditions.IsFalse(got, conditionType) {
			t.Errorf("%s not false: %+v", conditionType, got.Status.Conditions)
		}
	}
	if got.Status.Initialization != nil && ptr.Deref(got.Status.Initialization.Provisioned, false) {
		t.Errorf("provisioned before ready")
	}
}

func TestNoOwnerClusterWaits(t *testing.T) {
	vc := ownedVCluster()
	vc.OwnerReferences = nil
	h, _ := reconcileOnce(t, vc)
	if h.upgrades != 0 {
		t.Errorf("helm upgrades without owner Cluster: %d", h.upgrades)
	}
}

func TestSummaryConditions(t *testing.T) {
	vc := ownedVCluster()
	for _, conditionType := range []string{
		infrastructurev1alpha1.HelmChartDeployedCondition,
		infrastructurev1alpha1.ControlPlaneInitializedCondition,
		infrastructurev1alpha1.KubeconfigReadyCondition,
	} {
		markTrue(vc, conditionType)
	}

	setSummaryConditions(vc)
	if !conditions.IsFalse(vc, infrastructurev1alpha1.ReadyCondition) {
		t.Errorf("Ready true before readyz")
	}

	vc.Status.Ready = true
	setSummaryConditions(vc)
	for _, conditionType := range []string{infrastructurev1alpha1.ReadyCondition, infrastructurev1alpha1.AvailableCondition} {
		if !conditions.IsTrue(vc, conditionType) {
			t.Errorf("%s not true: %+v", conditionType, vc.Status.Conditions)
		}
	}

	conditions.Set(vc, metav1.Condition{
		Type:    infrastructurev1alpha1.HelmChartDeployedCondition,
		Status:  metav1.ConditionFalse,
		Reason:  infrastructurev1alpha1.HelmDeployFailedReason,
		Message: "boom",
	})
	setSummaryConditions(vc)
	if got := conditions.Get(vc, infrastructurev1alpha1.ReadyCondition); got.Status != metav1.ConditionFalse || got.Reason != infrastructurev1alpha1.HelmDeployFailedReason {
		t.Errorf("Ready after a failed deploy: %+v", got)
	}
}

func TestNamespaceSelector(t *testing.T) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}
	h, _ := reconcileWith(t, Options{NamespaceSelector: labels.SelectorFromSet(labels.Set{"tenant": "no"})}, cluster, ownedVCluster())
	if h.upgrades != 0 {
		t.Errorf("helm upgrades outside the selected namespaces: %d", h.upgrades)
	}
	h, _ = reconcileWith(t, Options{NamespaceSelector: labels.SelectorFromSet(labels.Set{"tenant": "yes"})}, cluster, ownedVCluster())
	if h.upgrades != 1 {
		t.Errorf("helm upgrades in a selected namespace: %d, want 1", h.upgrades)
	}
}

// a vCluster that comes up: its kubeconfig and certs Secrets, its Service, its API server
func vClusterUp() []client.Object {
	kubeconfig := `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://localhost:8443"}}]
users: [{name: u, user: {client-certificate-data: Y2VydA==, client-key-data: a2V5}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
`
	return []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vc-vc1", Namespace: "ns"}, Data: map[string][]byte{"config": []byte(kubeconfig)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vc1-certs", Namespace: "ns"}, Data: map[string][]byte{"ca.crt": []byte("ca"), "ca.key": []byte("key")}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "vc1", Namespace: "ns"}, Spec: corev1.ServiceSpec{ClusterIP: "10.0.0.1"}},
	}
}

type fakeClientConfigGetter struct{}

func (fakeClientConfigGetter) NewForConfig(*rest.Config) (kubernetes.Interface, error) {
	return kubefake.NewClientset(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}}), nil
}

// One create, one helm revision: writing spec.controlPlaneEndpoint after the install bumps the
// generation, but the chart and its values are the same; a change of the values upgrades.
func TestHelmInstallerUpgradesOnlyOnChartChange(t *testing.T) {
	ctx := context.Background()
	key := types.NamespacedName{Namespace: "ns", Name: "vc1"}
	vc := ownedVCluster()
	vc.Generation = 1
	vc.Spec.HelmRelease = &infrastructurev1alpha1.VirtualClusterHelmRelease{Values: "a: 1"}
	h := &countingHelm{}
	c := fakeclient.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(append(vClusterUp(), vc, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}},
			&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}})...).
		WithStatusSubresource(&infrastructurev1alpha1.VCluster{}).
		Build()
	r := &GenericReconciler{
		Client:             c,
		HelmClient:         h,
		Log:                logr.Discard(),
		Scheme:             c.Scheme(),
		clusterKindExists:  true,
		ClientConfigGetter: fakeClientConfigGetter{},
		HTTPClientGetter:   NewHTTPClientGetter(),
		New:                func() GenericVCluster { return &infrastructurev1alpha1.VCluster{} },
	}
	// reconcile as the API server would: a spec change bumps the generation (the fake client
	// does not)
	reconcile := func(times int) *infrastructurev1alpha1.VCluster {
		t.Helper()
		got := &infrastructurev1alpha1.VCluster{}
		for range times {
			before := &infrastructurev1alpha1.VCluster{}
			if err := c.Get(ctx, key, before); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Log(err)
			}
			if err := c.Get(ctx, key, got); err != nil {
				t.Fatal(err)
			}
			if !equality.Semantic.DeepEqual(before.Spec, got.Spec) {
				got.Generation++
				if err := c.Update(ctx, got); err != nil {
					t.Fatal(err)
				}
			}
		}
		return got
	}

	got := reconcile(5)
	if got.Spec.ControlPlaneEndpoint == nil || got.Spec.ControlPlaneEndpoint.Host != "10.0.0.1" {
		t.Fatalf("controlPlaneEndpoint not written: %+v", got.Spec.ControlPlaneEndpoint)
	}
	if got.Generation < 2 {
		t.Fatalf("generation %d: the endpoint did not bump it", got.Generation)
	}
	if h.upgrades != 1 {
		t.Errorf("helm revisions after a create: %d, want 1", h.upgrades)
	}
	if got.Status.DeployedChartHash == "" {
		t.Errorf("no deployedChartHash")
	}

	got.Spec.HelmRelease.Values = "a: 2"
	got.Generation++
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcile(3)
	if h.upgrades != 2 {
		t.Errorf("helm revisions after a values change: %d, want 2", h.upgrades)
	}
}

// A VCluster a provider without the hash deployed: no upgrade for the new status field.
func TestHelmInstallerAdoptsDeployedChart(t *testing.T) {
	vc := ownedVCluster()
	vc.Generation, vc.Status.ObservedGeneration = 3, 3
	markTrue(vc, infrastructurev1alpha1.HelmChartDeployedCondition)
	h, got := reconcileOnce(t, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, vc)
	if h.upgrades != 0 {
		t.Errorf("helm upgrades of a deployed VCluster: %d, want 0", h.upgrades)
	}
	if got.Status.DeployedChartHash == "" {
		t.Errorf("no deployedChartHash")
	}
}

// The values may hold secrets: the install logs their hash, never the values.
func TestHelmInstallerLogsNoValues(t *testing.T) {
	var lines []string
	vc := ownedVCluster()
	vc.Spec.HelmRelease = &infrastructurev1alpha1.VirtualClusterHelmRelease{Values: "password: s3cr3t"}
	c := fakeclient.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(vc, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}},
			&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}).
		WithStatusSubresource(&infrastructurev1alpha1.VCluster{}).
		Build()
	h := &countingHelm{}
	r := &GenericReconciler{
		Client:            c,
		HelmClient:        h,
		Log:               funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{Verbosity: 10}),
		Scheme:            c.Scheme(),
		clusterKindExists: true,
		HTTPClientGetter:  NewHTTPClientGetter(),
		New:               func() GenericVCluster { return &infrastructurev1alpha1.VCluster{} },
	}
	for range 2 {
		_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "vc1"}})
	}
	if h.upgrades != 1 {
		t.Fatalf("helm upgrades: %d, want 1", h.upgrades)
	}
	logged := strings.Join(lines, "\n")
	if strings.Contains(logged, "s3cr3t") {
		t.Errorf("values logged:\n%s", logged)
	}
	if !strings.Contains(logged, `"valuesHash"="`+sha256Hex("password: s3cr3t")+`"`) {
		t.Errorf("no valuesHash logged:\n%s", logged)
	}
}

func TestRequireChartVersion(t *testing.T) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}
	h, got := reconcileWith(t, Options{RequireChartVersion: true}, cluster, ownedVCluster())
	if h.upgrades != 0 {
		t.Errorf("helm upgrades without a chart version: %d", h.upgrades)
	}
	if c := conditions.Get(got, infrastructurev1alpha1.HelmChartDeployedCondition); c == nil || c.Reason != infrastructurev1alpha1.HelmDeployFailedReason {
		t.Errorf("HelmChartDeployed: %+v", c)
	}
}

func fluxScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := testScheme(t)
	for _, gvk := range []schema.GroupVersionKind{helmReleaseGVK, helmRepositoryGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		list := gvk
		list.Kind += "List"
		scheme.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
	}
	return scheme
}

func fluxVCluster() *infrastructurev1alpha1.VCluster {
	vc := ownedVCluster()
	vc.Spec.HelmRelease = &infrastructurev1alpha1.VirtualClusterHelmRelease{
		Installer:     infrastructurev1alpha1.FluxInstaller,
		Chart:         infrastructurev1alpha1.VirtualClusterHelmChart{Repo: "oci://mirror.example/charts", Version: "0.37.6"},
		Values:        "controlPlane:\n  distro:\n    k8s:\n      enabled: true\n",
		PostRenderers: []runtime.RawExtension{{Raw: []byte(`{"kustomize":{"patches":[{"target":{"kind":"Deployment"},"patch":"[]"}]}}`)}},
	}
	return vc
}

type fluxEnv struct {
	t *testing.T
	c client.Client
	r *GenericReconciler
	h *countingHelm
}

func newFluxEnv(t *testing.T, objs ...client.Object) *fluxEnv {
	t.Helper()
	h := &countingHelm{}
	c := fakeclient.NewClientBuilder().
		WithScheme(fluxScheme(t)).
		WithObjects(append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}})...).
		WithStatusSubresource(&infrastructurev1alpha1.VCluster{}, newUnstructured(helmReleaseGVK, "", "")).
		Build()
	return &fluxEnv{t: t, c: c, h: h, r: &GenericReconciler{
		Client:            c,
		HelmClient:        h,
		HelmSecrets:       helm.NewSecrets(c),
		Log:               logr.Discard(),
		Scheme:            c.Scheme(),
		Options:           Options{Flux: FluxOptions{DriftDetection: "enabled"}},
		clusterKindExists: true,
		HTTPClientGetter:  NewHTTPClientGetter(),
		New:               func() GenericVCluster { return &infrastructurev1alpha1.VCluster{} },
	}}
}

func (e *fluxEnv) reconcile(times int) *infrastructurev1alpha1.VCluster {
	e.t.Helper()
	return e.reconcileErr(times, false)
}

// reconcileExpectErr is reconcile for a pass that fails: at least one Reconcile returns an error.
func (e *fluxEnv) reconcileExpectErr(times int) *infrastructurev1alpha1.VCluster {
	e.t.Helper()
	return e.reconcileErr(times, true)
}

func (e *fluxEnv) reconcileErr(times int, expectErr bool) *infrastructurev1alpha1.VCluster {
	e.t.Helper()
	var errs []error
	for range times {
		if _, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "vc1"}}); err != nil {
			errs = append(errs, err)
		}
	}
	switch {
	case expectErr && len(errs) == 0:
		e.t.Error("reconcile: no error, want one")
	case !expectErr && len(errs) > 0:
		e.t.Errorf("reconcile: %v", errs)
	}
	got := &infrastructurev1alpha1.VCluster{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, got); err != nil && !kerrors.IsNotFound(err) {
		e.t.Fatal(err)
	}
	return got
}

func (e *fluxEnv) helmRelease() *unstructured.Unstructured {
	e.t.Helper()
	hr := newUnstructured(helmReleaseGVK, "ns", "vc1")
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, hr); err != nil {
		e.t.Fatal(err)
	}
	return hr
}

func (e *fluxEnv) setHelmReleaseStatus(conditionType, status string) {
	e.t.Helper()
	hr := e.helmRelease()
	hr.Object["status"] = map[string]any{
		"observedGeneration": hr.GetGeneration(),
		"conditions":         []any{map[string]any{"type": conditionType, "status": status, "message": "from flux"}},
	}
	if err := e.c.Status().Update(context.Background(), hr); err != nil {
		e.t.Fatal(err)
	}
}

func TestFluxInstallerWritesHelmRelease(t *testing.T) {
	e := newFluxEnv(t, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, fluxVCluster())
	got := e.reconcile(2)
	if e.h.upgrades != 0 {
		t.Errorf("helm ran under the flux installer: %d", e.h.upgrades)
	}
	if c := conditions.Get(got, infrastructurev1alpha1.HelmChartDeployedCondition); c == nil || c.Reason != infrastructurev1alpha1.HelmReleaseNotReadyReason {
		t.Errorf("HelmChartDeployed before Flux is ready: %+v", c)
	}

	hr := e.helmRelease()
	for path, want := range map[string]any{
		"spec.releaseName":               "vc1",
		"spec.chart.spec.version":        "0.37.6",
		"spec.chart.spec.chart":          "vcluster",
		"spec.chart.spec.sourceRef.name": "vc1",
		"spec.driftDetection.mode":       "enabled",
		"spec.suspend":                   false,
	} {
		got, _, _ := unstructured.NestedFieldNoCopy(hr.Object, strings.Split(path, ".")...)
		if got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	if v, _, _ := unstructured.NestedBool(hr.Object, "spec", "values", "controlPlane", "distro", "k8s", "enabled"); !v {
		t.Errorf("values not passed: %v", hr.Object["spec"])
	}
	if pr, _, _ := unstructured.NestedSlice(hr.Object, "spec", "postRenderers"); len(pr) != 1 {
		t.Errorf("postRenderers: %v", pr)
	}
	if owner := metav1.GetControllerOf(hr); owner == nil || owner.Kind != "VCluster" {
		t.Errorf("HelmRelease owner: %+v", owner)
	}

	repo := newUnstructured(helmRepositoryGVK, "ns", "vc1")
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, repo); err != nil {
		t.Fatal(err)
	}
	if typ, _, _ := unstructured.NestedString(repo.Object, "spec", "type"); typ != "oci" {
		t.Errorf("HelmRepository type %q, want oci", typ)
	}

	e.setHelmReleaseStatus("Ready", "True")
	got = e.reconcile(1)
	if !conditions.IsTrue(got, infrastructurev1alpha1.HelmChartDeployedCondition) {
		t.Errorf("HelmChartDeployed after Ready: %+v", conditions.Get(got, infrastructurev1alpha1.HelmChartDeployedCondition))
	}
}

// A stalled HelmRelease fails the VCluster without an error: no backoff, a requeue in a minute
// (the HelmRelease watch reports a recovery sooner).
func TestFluxInstallerStalled(t *testing.T) {
	e := newFluxEnv(t, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, fluxVCluster())
	e.r.helmReleaseWatched = true
	e.reconcile(2)
	e.setHelmReleaseStatus("Stalled", "True")
	result, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "vc1"}})
	if err != nil || result.RequeueAfter != time.Minute {
		t.Errorf("Reconcile of a stalled HelmRelease: %+v, %v; want a requeue in a minute, no error", result, err)
	}
	got := e.reconcile(1)
	if c := conditions.Get(got, infrastructurev1alpha1.HelmChartDeployedCondition); c == nil || c.Reason != infrastructurev1alpha1.HelmDeployFailedReason {
		t.Errorf("HelmChartDeployed after Stalled: %+v", c)
	}
	if got.Status.Phase != infrastructurev1alpha1.VirtualClusterFailed {
		t.Errorf("phase %q, want Failed", got.Status.Phase)
	}
}

func TestFluxInstallerSuspendsWhilePaused(t *testing.T) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}
	e := newFluxEnv(t, cluster, fluxVCluster())
	e.reconcile(2)

	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "c1"}, cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.Paused = ptr.To(true)
	if err := e.c.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	e.reconcile(2)
	if s, _, _ := unstructured.NestedBool(e.helmRelease().Object, "spec", "suspend"); !s {
		t.Errorf("HelmRelease not suspended while paused")
	}

	cluster.Spec.Paused = ptr.To(false)
	if err := e.c.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	e.reconcile(2)
	if s, _, _ := unstructured.NestedBool(e.helmRelease().Object, "spec", "suspend"); s {
		t.Errorf("HelmRelease still suspended after unpause")
	}
}

func TestFluxInstallerDelete(t *testing.T) {
	e := newFluxEnv(t, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, fluxVCluster())
	e.reconcile(2)
	// a suspended HelmRelease must be resumed first, or helm-controller skips the uninstall
	if err := e.r.fluxSuspend(context.Background(), fluxVCluster(), true); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := unstructured.NestedBool(e.helmRelease().Object, "spec", "suspend"); !s {
		t.Fatal("HelmRelease not suspended before the delete")
	}

	vc := &infrastructurev1alpha1.VCluster{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, vc); err != nil {
		t.Fatal(err)
	}
	// helm-controller's finalizer: the HelmRelease stays until it has uninstalled the chart
	hr := e.helmRelease()
	hr.SetFinalizers([]string{"finalizers.fluxcd.io"})
	if err := e.c.Update(context.Background(), hr); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), vc); err != nil {
		t.Fatal(err)
	}
	e.reconcile(1)
	hr = e.helmRelease()
	if s, _, _ := unstructured.NestedBool(hr.Object, "spec", "suspend"); s {
		t.Errorf("HelmRelease deleted while suspended: helm-controller would skip the uninstall")
	}
	if hr.GetDeletionTimestamp() == nil {
		t.Errorf("HelmRelease not deleted")
	}
	e.reconcile(1)
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, vc); err != nil {
		t.Errorf("VCluster gone before the chart: %v", err)
	}

	// helm-controller uninstalled the chart
	hr.SetFinalizers(nil)
	if err := e.c.Update(context.Background(), hr); err != nil {
		t.Fatal(err)
	}
	e.reconcile(1)
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, vc); !kerrors.IsNotFound(err) {
		t.Errorf("VCluster still there: %v %v", err, vc.Finalizers)
	}
	if e.h.upgrades != 0 {
		t.Errorf("helm ran")
	}
}

// chartVolumes are PVCs the way the StatefulSet controller labels them (its selector), for VCluster
// vc1 with a deployed etcd and two replicas, plus the old unlabelled name and others to keep.
func chartVolumes() (deleted, kept []client.Object) {
	pvc := func(namespace, name string, labels map[string]string) client.Object {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}}
	}
	deleted = []client.Object{
		pvc("ns", "data-vc1-0", nil),
		pvc("ns", "data-vc1-1", map[string]string{"app": "vcluster", "release": "vc1"}),
		pvc("ns", "data-vc1-etcd-0", map[string]string{"app": "vcluster-etcd", "release": "vc1"}),
		pvc("ns", "data-vc1-etcd-1", map[string]string{"app": "vcluster-etcd", "release": "vc1"}),
	}
	kept = []client.Object{
		pvc("ns", "data-vc2-0", map[string]string{"app": "vcluster", "release": "vc2"}),
		pvc("ns", "data-vc2-etcd-0", map[string]string{"app": "vcluster-etcd", "release": "vc2"}),
		pvc("ns", "postgres-0", map[string]string{"app": "postgres", "release": "vc1"}),
		pvc("other", "data-vc1-etcd-0", map[string]string{"app": "vcluster-etcd", "release": "vc1"}),
	}
	return deleted, kept
}

func (e *fluxEnv) volumes() []string {
	e.t.Helper()
	list := &corev1.PersistentVolumeClaimList{}
	if err := e.c.List(context.Background(), list); err != nil {
		e.t.Fatal(err)
	}
	var names []string
	for _, pvc := range list.Items {
		names = append(names, pvc.Namespace+"/"+pvc.Name)
	}
	return names
}

func TestFluxInstallerDeletesVolumesAfterTheChart(t *testing.T) {
	deleted, kept := chartVolumes()
	e := newFluxEnv(t, append(append([]client.Object{&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, fluxVCluster()}, deleted...), kept...)...)
	e.reconcile(2)

	// helm-controller's finalizer: the HelmRelease stays until it has uninstalled the chart
	hr := e.helmRelease()
	hr.SetFinalizers([]string{"finalizers.fluxcd.io"})
	if err := e.c.Update(context.Background(), hr); err != nil {
		t.Fatal(err)
	}
	vc := &infrastructurev1alpha1.VCluster{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, vc); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), vc); err != nil {
		t.Fatal(err)
	}
	e.reconcile(2)
	if got := e.volumes(); len(got) != len(deleted)+len(kept) {
		t.Fatalf("volumes deleted before the chart: %v", got)
	}

	hr = e.helmRelease()
	hr.SetFinalizers(nil)
	if err := e.c.Update(context.Background(), hr); err != nil {
		t.Fatal(err)
	}
	e.reconcile(1)
	want := []string{"ns/data-vc2-0", "ns/data-vc2-etcd-0", "ns/postgres-0", "other/data-vc1-etcd-0"}
	if got := e.volumes(); !slices.Equal(sorted(got), want) {
		t.Errorf("volumes %v, want %v", got, want)
	}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, vc); !kerrors.IsNotFound(err) {
		t.Errorf("VCluster still there: %v %v", err, vc.Finalizers)
	}
}

// A HelmRelease removed by someone else (an adoption rolled back, a hand delete) is not the
// provider's uninstall: the chart may still run, so deleting the VCluster keeps every volume.
func TestFluxInstallerKeepsVolumesWithoutItsOwnDelete(t *testing.T) {
	deleted, kept := chartVolumes()
	e := newFluxEnv(t, append(append([]client.Object{&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, fluxVCluster()}, deleted...), kept...)...)
	e.reconcile(2)
	if err := e.c.Delete(context.Background(), e.helmRelease()); err != nil {
		t.Fatal(err)
	}
	vc := &infrastructurev1alpha1.VCluster{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, vc); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), vc); err != nil {
		t.Fatal(err)
	}
	e.reconcile(1)
	if got := e.volumes(); len(got) != len(deleted)+len(kept) {
		t.Errorf("volumes deleted without the provider's uninstall: %v", got)
	}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, vc); !kerrors.IsNotFound(err) {
		t.Errorf("VCluster still there: %v %v", err, vc.Finalizers)
	}
}

func TestHelmInstallerDeletesVolumes(t *testing.T) {
	deleted, kept := chartVolumes()
	vc := ownedVCluster()
	vc.DeletionTimestamp = ptr.To(metav1.Now())
	e := newFluxEnv(t, append(append([]client.Object{&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, vc}, deleted...), kept...)...)
	e.reconcile(1)
	want := []string{"ns/data-vc2-0", "ns/data-vc2-etcd-0", "ns/postgres-0", "other/data-vc1-etcd-0"}
	if got := e.volumes(); !slices.Equal(sorted(got), want) {
		t.Errorf("volumes %v, want %v", got, want)
	}
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

func TestPostRenderersNeedFlux(t *testing.T) {
	vc := fluxVCluster()
	vc.Spec.HelmRelease.Installer = infrastructurev1alpha1.HelmInstaller
	h, got := reconcileWith(t, Options{}, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, vc)
	if h.upgrades != 0 {
		t.Errorf("helm ran with postRenderers")
	}
	if c := conditions.Get(got, infrastructurev1alpha1.HelmChartDeployedCondition); c == nil || c.Reason != infrastructurev1alpha1.HelmDeployFailedReason {
		t.Errorf("HelmChartDeployed: %+v", c)
	}
}

func TestFluxInstallerCopiesLabelsAndReconcileRequests(t *testing.T) {
	vc := fluxVCluster()
	vc.Labels = map[string]string{"turba-env.dev/tenant": "alice"}
	vc.Annotations = map[string]string{
		"reconcile.fluxcd.io/requestedAt": "op-1",
		"reconcile.fluxcd.io/resetAt":     "op-1",
		"unrelated":                       "x",
	}
	e := newFluxEnv(t, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, vc)
	e.reconcile(2)

	hr := e.helmRelease()
	if hr.GetLabels()["turba-env.dev/tenant"] != "alice" {
		t.Errorf("HelmRelease labels: %v", hr.GetLabels())
	}
	a := hr.GetAnnotations()
	if a["reconcile.fluxcd.io/requestedAt"] != "op-1" || a["reconcile.fluxcd.io/resetAt"] != "op-1" {
		t.Errorf("HelmRelease annotations: %v", a)
	}
	if _, ok := a["unrelated"]; ok {
		t.Errorf("unrelated annotation copied: %v", a)
	}

	repo := newUnstructured(helmRepositoryGVK, "ns", "vc1")
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, repo); err != nil {
		t.Fatal(err)
	}
	if repo.GetLabels()["turba-env.dev/tenant"] != "alice" {
		t.Errorf("HelmRepository labels: %v", repo.GetLabels())
	}
	ra := repo.GetAnnotations()
	if ra["reconcile.fluxcd.io/requestedAt"] != "op-1" {
		t.Errorf("HelmRepository annotations: %v", ra)
	}
	if _, ok := ra["reconcile.fluxcd.io/resetAt"]; ok {
		t.Errorf("resetAt on the HelmRepository: %v", ra)
	}

	// a new request on the VCluster reaches the HelmRelease
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, vc); err != nil {
		t.Fatal(err)
	}
	vc.Annotations["reconcile.fluxcd.io/requestedAt"] = "op-2"
	if err := e.c.Update(context.Background(), vc); err != nil {
		t.Fatal(err)
	}
	e.reconcile(1)
	if got := e.helmRelease().GetAnnotations()["reconcile.fluxcd.io/requestedAt"]; got != "op-2" {
		t.Errorf("requestedAt after the second request: %q", got)
	}
	// a label removed from the VCluster goes from the HelmRelease and the HelmRepository; one
	// of the HelmRelease's own stays
	hr = e.helmRelease()
	hr.SetLabels(map[string]string{"turba-env.dev/tenant": "alice", "own": "yes"})
	if err := e.c.Update(context.Background(), hr); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, vc); err != nil {
		t.Fatal(err)
	}
	vc.Labels = map[string]string{"team": "a"}
	if err := e.c.Update(context.Background(), vc); err != nil {
		t.Fatal(err)
	}
	e.reconcile(1)
	if l := e.helmRelease().GetLabels(); l["turba-env.dev/tenant"] != "" || l["team"] != "a" || l["own"] != "yes" {
		t.Errorf("HelmRelease labels after the VCluster's changed: %v", l)
	}
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(repo), repo); err != nil {
		t.Fatal(err)
	}
	if l := repo.GetLabels(); l["turba-env.dev/tenant"] != "" || l["team"] != "a" {
		t.Errorf("HelmRepository labels after the VCluster's changed: %v", l)
	}
}

func TestFluxDriftDetectionPerVCluster(t *testing.T) {
	vc := fluxVCluster()
	vc.Spec.HelmRelease.DriftDetection = "warn"
	e := newFluxEnv(t, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, vc)
	e.reconcile(2)
	if mode, _, _ := unstructured.NestedString(e.helmRelease().Object, "spec", "driftDetection", "mode"); mode != "warn" {
		t.Errorf("driftDetection.mode %q, want the VCluster's warn over the provider's enabled", mode)
	}

	// the owner turns it on for a force sync
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, vc); err != nil {
		t.Fatal(err)
	}
	vc.Spec.HelmRelease.DriftDetection = "enabled"
	if err := e.c.Update(context.Background(), vc); err != nil {
		t.Fatal(err)
	}
	e.reconcile(1)
	if mode, _, _ := unstructured.NestedString(e.helmRelease().Object, "spec", "driftDetection", "mode"); mode != "enabled" {
		t.Errorf("driftDetection.mode %q after the change, want enabled", mode)
	}
}

func TestFluxRepositoryProvider(t *testing.T) {
	// a shared repository someone else made, with a field the provider doesn't write
	shared := newUnstructured(helmRepositoryGVK, "ns", "mirror")
	shared.Object["spec"] = map[string]any{"url": "oci://old.example/charts", "secretRef": map[string]any{"name": "pull"}}
	e := newFluxEnv(t, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, fluxVCluster(), shared)
	e.r.Options.Flux.RepositoryName = "mirror"
	e.r.Options.Flux.RepositoryProvider = "gcp"
	e.reconcile(2)

	repo := newUnstructured(helmRepositoryGVK, "ns", "mirror")
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "mirror"}, repo); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]any{
		"spec.url":            "oci://mirror.example/charts",
		"spec.type":           "oci",
		"spec.provider":       "gcp",
		"spec.secretRef.name": "pull",
	} {
		got, _, _ := unstructured.NestedFieldNoCopy(repo.Object, strings.Split(path, ".")...)
		if got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	if name, _, _ := unstructured.NestedString(e.helmRelease().Object, "spec", "chart", "spec", "sourceRef", "name"); name != "mirror" {
		t.Errorf("sourceRef %q", name)
	}
}

func TestFluxRepositoryProviderOnlyForOCI(t *testing.T) {
	vc := fluxVCluster()
	vc.Spec.HelmRelease.Chart.Repo = "https://charts.example"
	e := newFluxEnv(t, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, vc)
	e.r.Options.Flux.RepositoryProvider = "gcp"
	e.reconcile(2)
	repo := newUnstructured(helmRepositoryGVK, "ns", "vc1")
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, repo); err != nil {
		t.Fatal(err)
	}
	spec, _, _ := unstructured.NestedMap(repo.Object, "spec")
	if _, ok := spec["provider"]; ok {
		t.Errorf("provider on an HTTP repository: %v", spec)
	}
	if _, ok := spec["type"]; ok {
		t.Errorf("type on an HTTP repository: %v", spec)
	}
}

// foreignHelmRelease is a HelmRelease named after the VCluster that another controller owns
// (an adoption that failed): the provider must neither suspend nor delete it.
func foreignHelmRelease() *unstructured.Unstructured {
	hr := newUnstructured(helmReleaseGVK, "ns", "vc1")
	hr.Object["spec"] = map[string]any{"suspend": false}
	hr.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "kustomize.toolkit.fluxcd.io/v1",
		Kind:       "Kustomization",
		Name:       "tenant",
		UID:        "other",
		Controller: ptr.To(true),
	}})
	return hr
}

func TestFluxInstallerLeavesForeignHelmRelease(t *testing.T) {
	deleted, kept := chartVolumes()
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}, Spec: clusterv1.ClusterSpec{Paused: ptr.To(true)}}
	e := newFluxEnv(t, append(append([]client.Object{cluster, fluxVCluster(), foreignHelmRelease()}, deleted...), kept...)...)
	e.reconcile(2)
	if s, _, _ := unstructured.NestedBool(e.helmRelease().Object, "spec", "suspend"); s {
		t.Errorf("foreign HelmRelease suspended")
	}

	vc := &infrastructurev1alpha1.VCluster{}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, vc); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "c1"}, cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.Paused = ptr.To(false)
	if err := e.c.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), vc); err != nil {
		t.Fatal(err)
	}
	e.reconcile(1)

	hr := e.helmRelease()
	if hr.GetDeletionTimestamp() != nil {
		t.Errorf("foreign HelmRelease deleted")
	}
	if owner := metav1.GetControllerOf(hr); owner == nil || owner.Kind != "Kustomization" {
		t.Errorf("foreign HelmRelease owner: %+v", owner)
	}
	if got := e.volumes(); len(got) != len(deleted)+len(kept) {
		t.Errorf("volumes deleted with a foreign HelmRelease: %v", got)
	}
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, vc); !kerrors.IsNotFound(err) {
		t.Errorf("VCluster still there: %v %v", err, vc.Finalizers)
	}
}

func TestFluxServiceAccount(t *testing.T) {
	e := newFluxEnv(t, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, fluxVCluster())
	e.reconcile(2)
	if _, ok, _ := unstructured.NestedString(e.helmRelease().Object, "spec", "serviceAccountName"); ok {
		t.Errorf("serviceAccountName without --flux-service-account")
	}
	e.r.Options.Flux.ServiceAccountName = "vcluster-installer"
	e.reconcile(1)
	if sa, _, _ := unstructured.NestedString(e.helmRelease().Object, "spec", "serviceAccountName"); sa != "vcluster-installer" {
		t.Errorf("serviceAccountName %q, want vcluster-installer", sa)
	}
}

// The installer that deployed a VCluster stays, whatever --installer says later.
func TestInstallerRecorded(t *testing.T) {
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}
	h, got := reconcileWith(t, Options{Installer: infrastructurev1alpha1.HelmInstaller}, cluster, ownedVCluster())
	if got.Status.Installer != infrastructurev1alpha1.HelmInstaller || h.upgrades != 1 {
		t.Errorf("status.installer %q, helm upgrades %d", got.Status.Installer, h.upgrades)
	}

	// deployed by flux, the provider flipped to helm since: still flux, helm never runs
	vc := ownedVCluster()
	vc.Status.Installer = infrastructurev1alpha1.FluxInstaller
	e := newFluxEnv(t, cluster.DeepCopy(), vc)
	e.r.Options.Installer = infrastructurev1alpha1.HelmInstaller
	e.reconcile(2)
	if e.h.upgrades != 0 {
		t.Errorf("helm ran for a VCluster flux deployed: %d", e.h.upgrades)
	}
	hr := e.helmRelease()
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "vc1"}, vc); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(context.Background(), vc); err != nil {
		t.Fatal(err)
	}
	e.reconcile(1)
	if err := e.c.Get(context.Background(), client.ObjectKeyFromObject(hr), hr); !kerrors.IsNotFound(err) {
		t.Errorf("HelmRelease after the delete: %v", err)
	}
}

// A failed helm upgrade returns its error for the backoff, without a RequeueAfter.
func TestHelmUpgradeFailureBacksOff(t *testing.T) {
	e := newFluxEnv(t, &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "ns"}}, ownedVCluster())
	e.h.fail = errors.New("boom")
	e.reconcile(1) // the Paused condition
	result, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "vc1"}})
	if err == nil || !result.IsZero() {
		t.Errorf("Reconcile of a failed upgrade: %+v, %v; want the error alone", result, err)
	}
	got := e.reconcileExpectErr(1)
	if c := conditions.Get(got, infrastructurev1alpha1.HelmChartDeployedCondition); c == nil || c.Reason != infrastructurev1alpha1.HelmDeployFailedReason {
		t.Errorf("HelmChartDeployed: %+v", c)
	}
}
