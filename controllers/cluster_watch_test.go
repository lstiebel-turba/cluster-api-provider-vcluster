package controllers

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/event"

	controlplanev1alpha1 "github.com/loft-sh/cluster-api-provider-vcluster/api/controlplane/v1alpha1"
	infrastructurev1alpha1 "github.com/loft-sh/cluster-api-provider-vcluster/api/infrastructure/v1alpha1"
)

func TestClusterToVCluster(t *testing.T) {
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "c1"},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{APIGroup: infrastructurev1alpha1.GroupVersion.Group, Kind: "VCluster", Name: "infra"},
			ControlPlaneRef:   clusterv1.ContractVersionedObjectReference{APIGroup: controlplanev1alpha1.GroupVersion.Group, Kind: "VCluster", Name: "cp"},
		},
	}
	for _, tc := range []struct {
		gvk  schema.GroupVersionKind
		want string
	}{
		{infrastructurev1alpha1.GroupVersion.WithKind("VCluster"), "infra"},
		{controlplanev1alpha1.GroupVersion.WithKind("VCluster"), "cp"},
	} {
		requests := clusterToVCluster(tc.gvk)(context.Background(), cluster)
		if len(requests) != 1 || requests[0].Namespace != "ns" || requests[0].Name != tc.want {
			t.Errorf("%s: requests %v, want ns/%s", tc.gvk, requests, tc.want)
		}
	}
}

func TestClusterPauseChanged(t *testing.T) {
	p := clusterPauseChanged()
	cluster := func(paused bool, annotation bool) *clusterv1.Cluster {
		c := &clusterv1.Cluster{Spec: clusterv1.ClusterSpec{Paused: ptr.To(paused)}}
		if annotation {
			c.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
		}
		return c
	}
	for _, tc := range []struct {
		name     string
		old, new *clusterv1.Cluster
		want     bool
	}{
		{"paused", cluster(false, false), cluster(true, false), true},
		{"unpaused", cluster(true, false), cluster(false, false), true},
		{"annotated", cluster(false, false), cluster(false, true), true},
		{"unchanged", cluster(true, false), cluster(true, false), false},
	} {
		if got := p.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new}); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if p.Create(event.CreateEvent{Object: cluster(true, false)}) {
		t.Error("create passed")
	}
}
