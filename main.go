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

package main

import (
	"flag"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	controlplanev1alpha1 "github.com/loft-sh/cluster-api-provider-vcluster/api/controlplane/v1alpha1"
	infrastructurev1alpha1 "github.com/loft-sh/cluster-api-provider-vcluster/api/infrastructure/v1alpha1"
	"github.com/loft-sh/cluster-api-provider-vcluster/controllers"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(infrastructurev1alpha1.AddToScheme(scheme))
	utilruntime.Must(controlplanev1alpha1.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var namespace string
	var namespaceSelector string
	var requireChartVersion bool
	var installer string
	var fluxRepositoryName string
	var fluxDriftDetection string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&namespace, "namespace", "", "The namespaces watched by the controller manager, comma-separated. Empty watches all.")
	flag.StringVar(&namespaceSelector, "namespace-selector", "", "Label selector a VCluster's namespace must match to be reconciled, for example turba-env.dev/environment.")
	flag.BoolVar(&requireChartVersion, "require-chart-version", false, "Refuse a VCluster without spec.helmRelease.chart.version instead of installing the latest chart.")
	flag.StringVar(&installer, "installer", "helm", "The installer of a VCluster without spec.helmRelease.installer: helm (the helm CLI) or flux (a Flux HelmRelease).")
	flag.StringVar(&fluxRepositoryName, "flux-repository-name", "", "flux installer: a HelmRepository shared by a namespace's VClusters. Empty: one per VCluster.")
	flag.StringVar(&fluxDriftDetection, "flux-drift-detection", "enabled", "flux installer: the HelmRelease's driftDetection.mode (enabled, warn, disabled).")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	var namespaces map[string]cache.Config
	for _, ns := range strings.Split(namespace, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			if namespaces == nil {
				namespaces = map[string]cache.Config{}
			}
			namespaces[ns] = cache.Config{}
		}
	}

	if installer != string(infrastructurev1alpha1.HelmInstaller) && installer != string(infrastructurev1alpha1.FluxInstaller) {
		setupLog.Error(nil, "--installer must be helm or flux", "installer", installer)
		os.Exit(1)
	}

	selector, err := labels.Parse(namespaceSelector)
	if err != nil {
		setupLog.Error(err, "invalid --namespace-selector")
		os.Exit(1)
	}

	mgr, err := manager.New(ctrl.GetConfigOrDie(), manager.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		WebhookServer: webhook.NewServer(webhook.Options{
			Port: 9443,
		}),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "4012c7fa.cluster.x-k8s.io",
		Cache: cache.Options{
			DefaultNamespaces: namespaces,
		},
		// Read these straight from the API server: a cache would watch them in every namespace
		// the manager watches, Secrets included.
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{
					&corev1.Secret{},
					&corev1.ConfigMap{},
					&corev1.Service{},
					&corev1.PersistentVolumeClaim{},
					&corev1.Namespace{},
				},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	if err := controllers.RegisterControllers(ctx, mgr, controllers.Options{
		NamespaceSelector:   selector,
		RequireChartVersion: requireChartVersion,
		Installer:           infrastructurev1alpha1.Installer(installer),
		Flux: controllers.FluxOptions{
			RepositoryName: fluxRepositoryName,
			DriftDetection: fluxDriftDetection,
		},
	}); err != nil {
		setupLog.Error(err, "unable to register controllers")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
