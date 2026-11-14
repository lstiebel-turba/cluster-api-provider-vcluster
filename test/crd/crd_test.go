// Package crd installs the generated CRDs into a real API server (envtest): a CEL rule the API
// server can't compile, or a schema it rejects, fails here instead of on a cluster.
package crd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestCRDsInstall(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set (setup-envtest)")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// the installer rule: may be set once, never changed
	vc := &unstructured.Unstructured{}
	vc.SetGroupVersionKind(schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Kind: "VCluster"})
	vc.SetNamespace(metav1.NamespaceDefault)
	vc.SetName("vc1")
	vc.Object["spec"] = map[string]any{"helmRelease": map[string]any{"installer": "flux"}}
	if err := c.Create(ctx, vc); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := unstructured.SetNestedField(vc.Object, "helm", "spec", "helmRelease", "installer"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, vc); err == nil {
		t.Error("installer changed from flux to helm: want the immutability rule to refuse")
	}
}
