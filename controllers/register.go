package controllers

import (
	"context"
	"fmt"

	"github.com/loft-sh/cluster-api-provider-vcluster/pkg/helm"
	"github.com/loft-sh/cluster-api-provider-vcluster/pkg/util/kubeconfighelper"
	"github.com/loft-sh/log/logr"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	controlplanev1alpha1 "github.com/loft-sh/cluster-api-provider-vcluster/api/controlplane/v1alpha1"
	infrastructurev1alpha1 "github.com/loft-sh/cluster-api-provider-vcluster/api/infrastructure/v1alpha1"
)

// Options configures the VCluster controllers.
type Options struct {
	// NamespaceSelector selects the namespaces whose VClusters are reconciled. Nil selects all.
	NamespaceSelector labels.Selector

	// RequireChartVersion refuses a VCluster without a chart version.
	RequireChartVersion bool

	// Installer is the installer of a VCluster without spec.helmRelease.installer. Empty: helm.
	Installer infrastructurev1alpha1.Installer

	// Flux configures the flux installer.
	Flux FluxOptions

	// Concurrency is the number of VClusters each controller reconciles at once. 0: 1.
	Concurrency int
}

func RegisterControllers(_ context.Context, mgr manager.Manager, options Options) error {
	rawConfig, err := kubeconfighelper.ConvertRestConfigToRawConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("unable to get config: %w", err)
	}

	log, err := logr.NewLoggerWithOptions(
		logr.WithOptionsFromEnv(),
		logr.WithComponentName("vcluster-controller"),
	)
	if err != nil {
		return fmt.Errorf("unable to setup logger: %w", err)
	}

	// infrastructure vCluster
	if err = (&GenericReconciler{
		ControllerName:     "infrastructure-vcluster",
		Client:             mgr.GetClient(),
		HelmClient:         helm.NewClient(rawConfig),
		HelmSecrets:        helm.NewSecrets(mgr.GetClient()),
		Log:                log,
		Scheme:             mgr.GetScheme(),
		ClientConfigGetter: NewClientConfigGetter(),
		HTTPClientGetter:   NewHTTPClientGetter(),
		Options:            options,
		New: func() GenericVCluster {
			return &infrastructurev1alpha1.VCluster{}
		},
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create controller: %w", err)
	}

	// controlplane vCluster
	if err = (&GenericReconciler{
		ControllerName:     "controlplane-vcluster",
		Client:             mgr.GetClient(),
		HelmClient:         helm.NewClient(rawConfig),
		HelmSecrets:        helm.NewSecrets(mgr.GetClient()),
		Log:                log,
		Scheme:             mgr.GetScheme(),
		ClientConfigGetter: NewClientConfigGetter(),
		HTTPClientGetter:   NewHTTPClientGetter(),
		Options:            options,
		New: func() GenericVCluster {
			return &controlplanev1alpha1.VCluster{}
		},
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create controller: %w", err)
	}

	return nil
}
