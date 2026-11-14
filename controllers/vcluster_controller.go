/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/paused"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1alpha1 "github.com/loft-sh/cluster-api-provider-vcluster/api/infrastructure/v1alpha1"
	"github.com/loft-sh/cluster-api-provider-vcluster/pkg/constants"
	"github.com/loft-sh/cluster-api-provider-vcluster/pkg/helm"
	"github.com/loft-sh/cluster-api-provider-vcluster/pkg/util/kubeconfighelper"
)

// GenericReconciler reconciles a VCluster object
type GenericReconciler struct {
	Client             client.Client
	HelmClient         helm.Client
	HelmSecrets        *helm.Secrets
	Log                logr.Logger
	Scheme             *runtime.Scheme
	ClientConfigGetter ClientConfigGetter
	HTTPClientGetter   HTTPClientGetter
	ControllerName     string
	Options            Options
	clusterKindExists  bool

	New func() GenericVCluster
}

type GenericVCluster interface {
	client.Object
	conditions.Setter

	GetSpec() *infrastructurev1alpha1.VClusterSpec
	GetStatus() *infrastructurev1alpha1.VClusterStatus
}

// The provider's own API calls; make manifests writes them to config/rbac/role.yaml. The helm
// installer's rights (the chart's objects, the release Secrets) are in config/rbac/helm-installer.
//
// VClusters of both groups: the cache watches them, finalizers by update, the rest by patch;
// finalizers also for the blockOwnerDeletion owner references on the HelmRelease and HelmRepository.
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io;controlplane.cluster.x-k8s.io,resources=vclusters,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io;controlplane.cluster.x-k8s.io,resources=vclusters/status,verbs=patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io;controlplane.cluster.x-k8s.io,resources=vclusters/finalizers,verbs=update
// The owner Cluster (cached) and the VCluster's namespace (uncached, for the namespace selector).
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get
// Uncached, in the VCluster's namespace: the vCluster's kubeconfig and certs Secrets (read), the
// CAPI kubeconfig and CA Secrets (written), the vCluster Service (host discovery), the chart's
// PersistentVolumeClaims (deleted with the VCluster).
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;patch
//+kubebuilder:rbac:groups="",resources=services,verbs=get
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=list;delete

func (r *GenericReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	r.Log.Info("Reconcile vCluster", "namespacedName", req.NamespacedName)

	// get virtual cluster object
	vCluster := r.New()
	err := r.Client.Get(ctx, req.NamespacedName, vCluster)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	// only VClusters in selected namespaces
	namespace := &corev1.Namespace{}
	err = r.Client.Get(ctx, types.NamespacedName{Name: req.Namespace}, namespace)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.Options.NamespaceSelector != nil && !r.Options.NamespaceSelector.Matches(labels.Set(namespace.Labels)) {
		return ctrl.Result{}, nil
	}

	// the owner Cluster, nil without Cluster API or before the Cluster controller set the
	// owner reference
	cluster, err := r.ownerCluster(ctx, vCluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	// is deleting?
	if vCluster.GetDeletionTimestamp() != nil {
		// check if namespace is deleting
		if namespace.DeletionTimestamp != nil {
			return ctrl.Result{}, RemoveFinalizer(ctx, r.Client, vCluster, CleanupFinalizer)
		}

		if isPaused(cluster, vCluster) {
			return ctrl.Result{}, nil
		}

		if r.installer(vCluster) == infrastructurev1alpha1.FluxInstaller {
			gone, err := r.fluxRemove(ctx, vCluster)
			if err != nil || !gone {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		} else {
			err = deleteHelmChart(ctx, r.HelmClient, r.HelmSecrets, req.Namespace, req.Name, r.Log)
			if err != nil {
				return ctrl.Result{}, err
			}
		}

		// the chart is gone: its volumes go too
		if err := r.deleteVolumes(ctx, vCluster); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, RemoveFinalizer(ctx, r.Client, vCluster, CleanupFinalizer)
	}

	switch {
	case cluster != nil:
		isPaused, requeue, err := paused.EnsurePausedCondition(ctx, r.Client, cluster, vCluster)
		if err == nil && isPaused && r.installer(vCluster) == infrastructurev1alpha1.FluxInstaller {
			// paused: Flux must not undo what the owner of the pause does (scaling to zero)
			err = r.fluxSuspend(ctx, vCluster, true)
		}
		if err != nil || isPaused || requeue {
			return ctrl.Result{}, err
		}
	case r.clusterKindExists:
		// as per CAPI docs:
		// The cluster controller will set an OwnerReference on the infrastructureCluster.
		// This controller should normally take no action during reconciliation until it sees the OwnerReference.
		return ctrl.Result{}, nil
	case annotations.HasPaused(vCluster):
		if r.installer(vCluster) == infrastructurev1alpha1.FluxInstaller {
			return ctrl.Result{}, r.fluxSuspend(ctx, vCluster, true)
		}
		return ctrl.Result{}, nil
	}

	// ensure finalizer
	err = EnsureFinalizer(ctx, r.Client, vCluster, CleanupFinalizer)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure finalizer: %w", err)
	}

	// Initialize the patch helper.
	patchHelper, err := patch.NewHelper(vCluster, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		// Always reconcile the summary conditions and the Status.Phase field.
		setSummaryConditions(vCluster)
		r.reconcilePhase(vCluster)

		// Always attempt to Patch the Cluster object and status after each reconciliation.
		// Patch ObservedGeneration only if the reconciliation completed successfully
		patchOpts := []patch.Option{}
		if reterr == nil {
			patchOpts = append(patchOpts, patch.WithStatusObservedGeneration{})
		}
		if err := patchCluster(ctx, patchHelper, vCluster, patchOpts...); err != nil {
			reterr = utilerrors.NewAggregate([]error{reterr, err})
		}
	}()

	// check if we have to redeploy
	err = r.redeployIfNeeded(ctx, vCluster)
	if err == nil && !conditions.IsTrue(vCluster, infrastructurev1alpha1.HelmChartDeployedCondition) {
		// the flux installer: helm-controller still installing or upgrading
		return ctrl.Result{RequeueAfter: time.Second * 5}, nil
	}
	if err != nil {
		r.Log.Error(err, "error during virtual cluster deploy",
			"namespace", vCluster.GetNamespace(),
			"name", vCluster.GetName(),
		)
		conditions.Set(vCluster, metav1.Condition{
			Type:    infrastructurev1alpha1.HelmChartDeployedCondition,
			Status:  metav1.ConditionFalse,
			Reason:  infrastructurev1alpha1.HelmDeployFailedReason,
			Message: err.Error(),
		})
		return ctrl.Result{RequeueAfter: time.Second * 5}, err
	}

	// check if vcluster is initialized and sync the kubeconfig Secret
	restConfig, err := r.syncVClusterKubeconfig(ctx, vCluster)
	if err != nil {
		r.Log.Info("vCluster is not ready",
			"namespace", vCluster.GetNamespace(),
			"name", vCluster.GetName(),
			"err", err,
		)
		conditions.Set(vCluster, metav1.Condition{
			Type:    infrastructurev1alpha1.KubeconfigReadyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  infrastructurev1alpha1.CheckFailedReason,
			Message: err.Error(),
		})
		return ctrl.Result{RequeueAfter: time.Second * 5}, nil
	}

	// sync the infrastructure cluster
	err = r.patchInfrastructureCluster(ctx, cluster, vCluster)
	if err != nil {
		r.Log.Error(err, "error during infrastructure cluster patch",
			"namespace", vCluster.GetNamespace(),
			"name", vCluster.GetName(),
		)
		conditions.Set(vCluster, metav1.Condition{
			Type:    infrastructurev1alpha1.InfrastructureClusterSyncedCondition,
			Status:  metav1.ConditionFalse,
			Reason:  infrastructurev1alpha1.PatchFailedReason,
			Message: err.Error(),
		})
		return ctrl.Result{}, err
	}
	markTrue(vCluster, infrastructurev1alpha1.InfrastructureClusterSyncedCondition)

	// check if vcluster is ready
	vCluster.GetStatus().Ready, err = r.checkReadyz(vCluster, restConfig)
	if vCluster.GetStatus().Ready {
		// provisioned never goes back to false (Cluster API v1beta2 contract)
		initialization(vCluster).Provisioned = ptr.To(true)
	}
	if err != nil || !vCluster.GetStatus().Ready {
		r.Log.V(1).Info("readiness check failed", "err", err)
		return ctrl.Result{RequeueAfter: time.Second * 5}, nil
	}

	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// chartVolumeApps are the app labels of the vCluster chart's StatefulSets: the control plane
// (its volume holds embedded etcd or SQLite) and a deployed etcd.
var chartVolumeApps = []string{"vcluster", "vcluster-etcd"}

// deleteVolumes deletes the PersistentVolumeClaims of the VCluster's chart, once the chart is
// uninstalled: the StatefulSet controller labels them with the StatefulSet's selector (app and
// release=<name>), whatever their names (data-<name>-0, data-<name>-etcd-0, more replicas).
// data-<name>-0 by name too, for charts whose volumes lack these labels.
func (r *GenericReconciler) deleteVolumes(ctx context.Context, vCluster GenericVCluster) error {
	names := sets.New("data-" + vCluster.GetName() + "-0")
	for _, app := range chartVolumeApps {
		list := &corev1.PersistentVolumeClaimList{}
		err := r.Client.List(ctx, list, client.InNamespace(vCluster.GetNamespace()),
			client.MatchingLabels{"app": app, "release": vCluster.GetName()})
		if err != nil {
			return fmt.Errorf("list volumes: %w", err)
		}
		for _, pvc := range list.Items {
			names.Insert(pvc.Name)
		}
	}
	for _, name := range sets.List(names) {
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: vCluster.GetNamespace()}}
		if err := r.Client.Delete(ctx, pvc); err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("delete volume %s: %w", name, err)
		}
	}
	return nil
}

func (r *GenericReconciler) reconcilePhase(vCluster GenericVCluster) {
	logger := r.Log
	oldPhase := vCluster.GetStatus().Phase

	// Check for failed state first
	vCluster.GetStatus().Phase = infrastructurev1alpha1.VirtualClusterPending
	vCluster.GetStatus().Reason = ""
	vCluster.GetStatus().Message = ""
	for _, condition := range vCluster.GetStatus().Conditions {
		if condition.Status == metav1.ConditionFalse && isFailureReason(condition.Reason) {
			vCluster.GetStatus().Phase = infrastructurev1alpha1.VirtualClusterFailed
			vCluster.GetStatus().Reason = condition.Reason
			vCluster.GetStatus().Message = condition.Message
			break
		}
	}

	// If not failed, check if deployed or pending
	if vCluster.GetStatus().Phase != infrastructurev1alpha1.VirtualClusterFailed {
		if vCluster.GetStatus().Ready && conditions.IsTrue(vCluster, infrastructurev1alpha1.ControlPlaneInitializedCondition) {
			vCluster.GetStatus().Phase = infrastructurev1alpha1.VirtualClusterDeployed
		} else {
			vCluster.GetStatus().Phase = infrastructurev1alpha1.VirtualClusterPending
		}
	}

	// Log phase transitions
	if oldPhase != vCluster.GetStatus().Phase {
		logger.Info("vcluster phase changed",
			"namespace", vCluster.GetNamespace(),
			"name", vCluster.GetName(),
			"oldPhase", oldPhase,
			"newPhase", vCluster.GetStatus().Phase,
			"reason", vCluster.GetStatus().Reason,
			"message", vCluster.GetStatus().Message,
		)
	}
}

// installer returns the VCluster's installer: its own, else the provider's default, else helm.
func (r *GenericReconciler) installer(vCluster GenericVCluster) infrastructurev1alpha1.Installer {
	if hr := vCluster.GetSpec().HelmRelease; hr != nil && hr.Installer != "" {
		return hr.Installer
	}
	if r.Options.Installer != "" {
		return r.Options.Installer
	}
	return infrastructurev1alpha1.HelmInstaller
}

func (r *GenericReconciler) chartSpec(vCluster GenericVCluster) (chartSpec, error) {
	hr := vCluster.GetSpec().HelmRelease
	chart := chartSpec{repo: constants.DefaultVClusterRepo, name: constants.DefaultVClusterChartName}
	if hr == nil {
		hr = &infrastructurev1alpha1.VirtualClusterHelmRelease{}
	}
	if hr.Chart.Repo != "" {
		chart.repo = hr.Chart.Repo
	}
	if hr.Chart.Name != "" {
		chart.name = hr.Chart.Name
	}
	// Remove 'v' prefix if present
	chart.version = strings.TrimPrefix(hr.Chart.Version, "v")
	if chart.version == "" && r.Options.RequireChartVersion {
		return chart, fmt.Errorf("spec.helmRelease.chart.version is required")
	}

	if hr.Values != "" && hr.ValuesObject != nil {
		return chart, fmt.Errorf("both values and valuesObject cannot be set")
	}
	if hr.ValuesObject != nil {
		rawValues, err := json.Marshal(hr.ValuesObject)
		if err != nil {
			return chart, fmt.Errorf("error marshalling valuesObject: %w", err)
		}
		chart.values = string(rawValues)
	} else {
		chart.values = hr.Values
	}

	for _, raw := range hr.PostRenderers {
		var postRenderer any
		if err := json.Unmarshal(raw.Raw, &postRenderer); err != nil {
			return chart, fmt.Errorf("postRenderers: %w", err)
		}
		chart.postRenderers = append(chart.postRenderers, postRenderer)
	}
	if len(chart.postRenderers) > 0 && r.installer(vCluster) != infrastructurev1alpha1.FluxInstaller {
		return chart, fmt.Errorf("postRenderers need the flux installer")
	}
	return chart, nil
}

func (r *GenericReconciler) redeployIfNeeded(ctx context.Context, vCluster GenericVCluster) error {
	chart, err := r.chartSpec(vCluster)
	if err != nil {
		return err
	}

	// flux: write the HelmRelease every time, helm-controller decides whether to upgrade
	if r.installer(vCluster) == infrastructurev1alpha1.FluxInstaller {
		state, err := r.fluxDeploy(ctx, vCluster, chart)
		switch {
		case err != nil:
			return err
		case state.failed:
			return fmt.Errorf("HelmRelease stalled: %s", state.message)
		case !state.ready:
			conditions.Set(vCluster, metav1.Condition{
				Type:    infrastructurev1alpha1.HelmChartDeployedCondition,
				Status:  metav1.ConditionFalse,
				Reason:  infrastructurev1alpha1.HelmReleaseNotReadyReason,
				Message: state.message,
			})
			return nil
		}
		if !conditions.IsTrue(vCluster, infrastructurev1alpha1.HelmChartDeployedCondition) {
			markTrue(vCluster, infrastructurev1alpha1.HelmChartDeployedCondition)
			conditions.Delete(vCluster, infrastructurev1alpha1.KubeconfigReadyCondition)
		}
		return nil
	}

	if vCluster.GetGeneration() == vCluster.GetStatus().ObservedGeneration &&
		conditions.IsTrue(vCluster, infrastructurev1alpha1.HelmChartDeployedCondition) {
		return nil
	}

	logger := r.Log
	chartRepo, chartName, chartVersion, values := chart.repo, chart.name, chart.version, chart.values

	if !conditions.IsTrue(vCluster, infrastructurev1alpha1.HelmChartDeployedCondition) {
		logger.Info("deploying vcluster",
			"namespace", vCluster.GetNamespace(),
			"clusterName", vCluster.GetName(),
			"chartRepo", chartRepo,
			"chartName", chartName,
			"chartVersion", chartVersion,
			"values", values,
		)
	} else {
		logger.Info("upgrading vcluster",
			"namespace", vCluster.GetNamespace(),
			"clusterName", vCluster.GetName(),
			"chartRepo", chartRepo,
			"chartName", chartName,
			"chartVersion", chartVersion,
			"values", values,
		)
	}

	var chartPath string
	if chartVersion != "" {
		chartPath = fmt.Sprintf("./%s-%s.tgz", chartName, chartVersion)
	} else {
		chartPath = fmt.Sprintf("./%s-latest.tgz", chartName)
	}

	_, err = os.Stat(chartPath)
	if err != nil {
		// we have to upgrade / install the chart
		err = r.HelmClient.Upgrade(vCluster.GetName(), vCluster.GetNamespace(), helm.UpgradeOptions{
			Chart:   chartName,
			Repo:    chartRepo,
			Version: chartVersion,
			Values:  values,
		})
	} else {
		// we have to upgrade / install the chart
		err = r.HelmClient.Upgrade(vCluster.GetName(), vCluster.GetNamespace(), helm.UpgradeOptions{
			Path:   chartPath,
			Values: values,
		})
	}
	if err != nil {
		if len(err.Error()) > 512 {
			err = fmt.Errorf("%v ... ", err.Error()[:512])
		}

		return fmt.Errorf("error installing / upgrading vcluster: %w", err)
	}

	markTrue(vCluster, infrastructurev1alpha1.HelmChartDeployedCondition)
	conditions.Delete(vCluster, infrastructurev1alpha1.KubeconfigReadyCondition)

	return nil
}

func (r *GenericReconciler) syncVClusterKubeconfig(ctx context.Context, vCluster GenericVCluster) (*rest.Config, error) {
	credentials, err := GetVClusterCredentials(ctx, r.Client, vCluster)
	if err != nil {
		return nil, err
	}

	restConfig, err := kubeconfighelper.NewVClusterClientConfig(vCluster.GetName(), vCluster.GetNamespace(), "", credentials.ClientCert, credentials.ClientKey)
	if err != nil {
		return nil, err
	}

	kubeClient, err := r.ClientConfigGetter.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()

	// if we haven't checked if the vcluster is initialized, do it now
	if !conditions.IsTrue(vCluster, infrastructurev1alpha1.ControlPlaneInitializedCondition) {
		_, err = kubeClient.CoreV1().ServiceAccounts("default").Get(ctxTimeout, "default", metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		markTrue(vCluster, infrastructurev1alpha1.ControlPlaneInitializedCondition)
	}
	// setting .Status.Initialized outside of the condition above to ensure
	// that it is set on old CRs, which were missing this field, as well
	vCluster.GetStatus().Initialized = true
	initialization(vCluster).ControlPlaneInitialized = ptr.To(true)

	// write kubeconfig to the vcluster.Name+"-kubeconfig" Secret as expected by CAPI convention
	kubeConfig, err := GetVClusterKubeConfig(ctx, r.Client, vCluster)
	if err != nil {
		return nil, fmt.Errorf("can not retrieve kubeconfig: %w", err)
	}
	if len(kubeConfig.Clusters) != 1 {
		return nil, fmt.Errorf("unexpected kube config")
	}

	// If vcluster.spec.controlPlaneEndpoint.Host is not set, try to autodiscover it from
	// the Service that targets vcluster pods, and write it back into the spec.
	controlPlaneEndpoint := vCluster.GetSpec().ControlPlaneEndpoint
	if controlPlaneEndpoint == nil {
		controlPlaneEndpoint = &clusterv1.APIEndpoint{}
	}

	controlPlaneHost := controlPlaneEndpoint.Host
	if controlPlaneHost == "" {
		controlPlaneHost, err = DiscoverHostFromService(ctx, r.Client, vCluster)
		if err != nil {
			return nil, err
		}

		// write the discovered host back into vCluster CR
		if vCluster.GetSpec().ControlPlaneEndpoint == nil {
			vCluster.GetSpec().ControlPlaneEndpoint = &clusterv1.APIEndpoint{}
		}
		vCluster.GetSpec().ControlPlaneEndpoint.Host = controlPlaneHost
		if vCluster.GetSpec().ControlPlaneEndpoint.Port == 0 {
			vCluster.GetSpec().ControlPlaneEndpoint.Port = DefaultControlPlanePort
		}
	}

	for k := range kubeConfig.Clusters {
		host := kubeConfig.Clusters[k].Server
		if controlPlaneHost != "" {
			if vCluster.GetSpec().ControlPlaneEndpoint.Port != 0 {
				host = fmt.Sprintf("%s:%d", controlPlaneHost, vCluster.GetSpec().ControlPlaneEndpoint.Port)
			} else {
				host = fmt.Sprintf("%s:%d", controlPlaneHost, DefaultControlPlanePort)
			}
		}
		if !strings.HasPrefix(host, "https://") {
			host = "https://" + host
		}
		kubeConfig.Clusters[k].Server = host
	}

	outKubeConfig, err := clientcmd.Write(*kubeConfig)
	if err != nil {
		return nil, err
	}

	// get cluster name
	clusterName := vCluster.GetName()
	if ownerRef := clusterOwnerRef(vCluster); r.clusterKindExists && ownerRef != nil {
		clusterName = ownerRef.Name
	}

	// create kubeconfig secret
	kubeSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-kubeconfig", clusterName),
			Namespace: vCluster.GetNamespace(),
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: clusterName,
			},
		},
	}
	_, err = controllerutil.CreateOrPatch(ctx, r.Client, kubeSecret, func() error {
		kubeSecret.Type = clusterv1.ClusterSecretType
		kubeSecret.Data = map[string][]byte{
			KubeconfigDataName: outKubeConfig,
		}
		// owned by the VCluster: deleted with it
		return controllerutil.SetControllerReference(vCluster, kubeSecret, r.Client.Scheme())
	})
	if err != nil {
		return nil, fmt.Errorf("can not create a kubeconfig secret: %w", err)
	}

	// create ca secret
	certsSecret := &corev1.Secret{}
	err = r.Client.Get(ctx, types.NamespacedName{Name: fmt.Sprintf("%s-certs", vCluster.GetName()), Namespace: vCluster.GetNamespace()}, certsSecret)
	if err != nil {
		return nil, fmt.Errorf("can not get certs secret: %w", err)
	}
	caSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-ca", clusterName),
			Namespace: vCluster.GetNamespace(),
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: clusterName,
			},
		},
	}
	_, err = controllerutil.CreateOrPatch(ctx, r.Client, caSecret, func() error {
		caSecret.Data = map[string][]byte{
			TLSKeyDataName: certsSecret.Data["ca.key"],
			TLSCrtDataName: certsSecret.Data["ca.crt"],
		}
		return controllerutil.SetControllerReference(vCluster, caSecret, r.Client.Scheme())
	})
	if err != nil {
		return nil, fmt.Errorf("can not create a ca secret: %w", err)
	}

	markTrue(vCluster, infrastructurev1alpha1.KubeconfigReadyCondition)
	return restConfig, nil
}

func (r *GenericReconciler) checkReadyz(vCluster GenericVCluster, restConfig *rest.Config) (bool, error) {
	t := time.Now()
	transport, err := rest.TransportFor(restConfig)
	if err != nil {
		return false, err
	}
	client := r.HTTPClientGetter.ClientFor(transport, 10*time.Second)
	resp, err := client.Get(fmt.Sprintf("https://%s:%d/readyz", vCluster.GetSpec().ControlPlaneEndpoint.Host, vCluster.GetSpec().ControlPlaneEndpoint.Port))
	r.Log.V(1).Info("ready check done", "namespace", vCluster.GetNamespace(), "name", vCluster.GetName(), "duration", time.Since(t))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	if string(body) != "ok" {
		return false, nil
	}

	return true, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GenericReconciler) SetupWithManager(mgr ctrl.Manager) error {
	var err error
	r.clusterKindExists, err = kindExists(mgr.GetConfig(), clusterv1.GroupVersion.WithKind("Cluster"))
	if err != nil {
		return err
	}

	b := ctrl.NewControllerManagedBy(mgr).
		For(r.New()).
		Named(r.ControllerName)
	if r.clusterKindExists {
		gvk, err := apiutil.GVKForObject(r.New(), mgr.GetScheme())
		if err != nil {
			return err
		}
		// a paused VCluster isn't requeued: the Cluster's pause and unpause must reach it
		b = b.Watches(&clusterv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(clusterToVCluster(gvk)),
			builder.WithPredicates(clusterPauseChanged()))
	}
	return b.Complete(r)
}

// clusterToVCluster maps a Cluster to its infrastructure and control plane references of kind gvk.
func clusterToVCluster(gvk schema.GroupVersionKind) handler.MapFunc {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		cluster, ok := obj.(*clusterv1.Cluster)
		if !ok {
			return nil
		}
		var requests []reconcile.Request
		for _, ref := range []clusterv1.ContractVersionedObjectReference{cluster.Spec.InfrastructureRef, cluster.Spec.ControlPlaneRef} {
			if ref.APIGroup == gvk.Group && ref.Kind == gvk.Kind && ref.Name != "" {
				requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: cluster.Namespace, Name: ref.Name}})
			}
		}
		return requests
	}
}

// clusterPauseChanged passes a Cluster update that pauses or unpauses it (spec.paused or the
// paused annotation).
func clusterPauseChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldCluster, ok1 := e.ObjectOld.(*clusterv1.Cluster)
			newCluster, ok2 := e.ObjectNew.(*clusterv1.Cluster)
			if !ok1 || !ok2 {
				return false
			}
			return ptr.Deref(oldCluster.Spec.Paused, false) != ptr.Deref(newCluster.Spec.Paused, false) ||
				annotations.HasPaused(oldCluster) != annotations.HasPaused(newCluster)
		},
	}
}
