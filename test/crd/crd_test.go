// Package crd installs the generated CRDs into a real API server (envtest): a CEL rule the API
// server can't compile, or a schema it rejects, fails here instead of on a cluster.
package crd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

	// the installer rule: set at create or never, never changed, added or removed
	for _, group := range []string{"infrastructure.cluster.x-k8s.io", "controlplane.cluster.x-k8s.io"} {
		for _, tc := range []struct {
			name     string
			from, to map[string]any // spec.helmRelease, nil: none
		}{
			{"changed", map[string]any{"installer": "flux"}, map[string]any{"installer": "helm"}},
			{"added", map[string]any{"values": "a: 1"}, map[string]any{"values": "a: 1", "installer": "flux"}},
			{"added with helmRelease", nil, map[string]any{"installer": "flux"}},
			{"removed", map[string]any{"installer": "flux"}, map[string]any{}},
			{"removed with helmRelease", map[string]any{"installer": "helm"}, nil},
		} {
			vc := &unstructured.Unstructured{}
			vc.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: "v1alpha1", Kind: "VCluster"})
			vc.SetNamespace(metav1.NamespaceDefault)
			vc.SetGenerateName("vc-")
			vc.Object["spec"] = helmReleaseSpec(tc.from)
			if err := c.Create(ctx, vc); err != nil {
				t.Fatalf("%s %s: create: %v", group, tc.name, err)
			}
			vc.Object["spec"] = helmReleaseSpec(tc.to)
			err := c.Update(ctx, vc)
			if err == nil || !strings.Contains(err.Error(), "installer is immutable") {
				t.Errorf("%s: installer %s: %v, want the immutability rule to refuse", group, tc.name, err)
			}

			// other changes pass
			vc.Object["spec"] = helmReleaseSpec(tc.from)
			if err := c.Get(ctx, client.ObjectKeyFromObject(vc), vc); err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedField(vc.Object, "10.0.0.1", "spec", "controlPlaneEndpoint", "host"); err != nil {
				t.Fatal(err)
			}
			if err := c.Update(ctx, vc); err != nil {
				t.Errorf("%s %s: another change refused: %v", group, tc.name, err)
			}
		}
	}
}

func helmReleaseSpec(helmRelease map[string]any) map[string]any {
	if helmRelease == nil {
		return map[string]any{}
	}
	return map[string]any{"helmRelease": helmRelease}
}
