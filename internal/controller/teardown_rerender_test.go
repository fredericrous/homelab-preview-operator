package controller

import (
	"context"
	"testing"

	logr "github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

func TestHashPreviewConfigSpec(t *testing.T) {
	if hashPreviewConfigSpec(nil) != "" {
		t.Error("nil config should hash to empty string")
	}
	a := &v1.PreviewConfig{Spec: v1.PreviewConfigSpec{
		DeploymentType:  v1.DeploymentTypeDeployment,
		DeploymentPatch: &v1.DeploymentPatch{RemoveContainers: []string{"litestream"}},
	}}
	b := &v1.PreviewConfig{Spec: v1.PreviewConfigSpec{
		DeploymentType:  v1.DeploymentTypeDeployment,
		DeploymentPatch: &v1.DeploymentPatch{RemoveContainers: []string{"litestream", "s3-prune"}},
	}}
	if hashPreviewConfigSpec(a) == "" {
		t.Fatal("expected a non-empty hash")
	}
	if hashPreviewConfigSpec(a) != hashPreviewConfigSpec(a) {
		t.Error("hash must be deterministic")
	}
	if hashPreviewConfigSpec(a) == hashPreviewConfigSpec(b) {
		t.Error("a spec change (added removeContainers) must change the hash")
	}
}

func snapScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	for _, k := range []string{"VolumeSnapshot", "VolumeSnapshotContent"} {
		s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: k}, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: k + "List"}, &unstructured.UnstructuredList{})
	}
	return s
}

func snapObj(kind, ns, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: kind})
	if ns != "" {
		o.SetNamespace(ns)
	}
	o.SetName(name)
	return o
}

func TestTeardownSnapshots(t *testing.T) {
	// App namespace carries the CNPG annotation so the CNPG snapshot in the
	// cluster namespace is also reaped.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "myapp",
			Annotations: map[string]string{
				"postgres.cnpg.io/cluster-name":      "postgres-apps",
				"postgres.cnpg.io/cluster-namespace": "postgres",
			},
		},
	}
	objs := []*unstructured.Unstructured{
		snapObj("VolumeSnapshotContent", "", "config-preview-myapp-pr-7-content"),
		snapObj("VolumeSnapshotContent", "", "postgres-preview-7-content"),
		snapObj("VolumeSnapshot", "myapp", "config-preview-pr-7"),
		snapObj("VolumeSnapshot", "postgres", "postgres-preview-pr-7"),
	}
	b := fake.NewClientBuilder().WithScheme(snapScheme()).WithObjects(ns)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	c := b.Build()
	h := &PreviewHandler{client: c, log: logr.Discard()}

	if err := h.TeardownSnapshots(context.Background(), "myapp", "7"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	for _, o := range objs {
		got := o.DeepCopy()
		err := c.Get(context.Background(), types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}, got)
		if !errors.IsNotFound(err) {
			t.Errorf("%s %s/%s should be deleted, got err=%v", o.GetKind(), o.GetNamespace(), o.GetName(), err)
		}
	}
	// Idempotent: a second teardown with everything already gone must not error.
	if err := h.TeardownSnapshots(context.Background(), "myapp", "7"); err != nil {
		t.Errorf("second teardown should be a no-op, got %v", err)
	}
}
