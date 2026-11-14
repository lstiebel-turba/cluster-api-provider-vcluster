package controllers

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1alpha1 "github.com/loft-sh/cluster-api-provider-vcluster/api/infrastructure/v1alpha1"
	"github.com/loft-sh/cluster-api-provider-vcluster/pkg/helm"
)

type countingHelm struct{ upgrades int }

func (c *countingHelm) Install(_, _ string, _ helm.UpgradeOptions) error { return nil }
func (c *countingHelm) Upgrade(_, _ string, _ helm.UpgradeOptions) error {
	c.upgrades++
	return nil
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
	for range 2 {
		// a failed deploy returns its error for the backoff; the conditions carry it too
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "vc1"}})
		if err != nil {
			t.Log(err)
		}
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
