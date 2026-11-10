package controller

import (
	"context"
	"testing"

	logr "github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func clonePVC(volumeName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "prowlarr-config-clone", Namespace: "preview-pr-1138"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeName:  volumeName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
}

func newPVCHandler(t *testing.T, objs ...client.Object) (*PreviewHandler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PreviewHandler{client: c, log: logr.Discard()}, c
}

func TestCreateIfAbsentCreatesAMissingClaim(t *testing.T) {
	h, c := newPVCHandler(t)
	if err := h.createIfAbsent(context.Background(), clonePVC("")); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "preview-pr-1138", Name: "prowlarr-config-clone"}, got); err != nil {
		t.Fatalf("claim should exist: %v", err)
	}
}

// A retry finds the clone already bound. It must be left exactly as it is: the old
// create-or-update path overwrote it with a request that had no volumeName, which the API
// server rejects as an immutable-spec change and which wedged the whole preview.
func TestCreateIfAbsentLeavesABoundClaimUntouched(t *testing.T) {
	bound := clonePVC("pvc-6bce613c")
	h, c := newPVCHandler(t, bound)

	if err := h.createIfAbsent(context.Background(), clonePVC("")); err != nil {
		t.Fatalf("a retry must not fail on an existing claim: %v", err)
	}
	got := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "preview-pr-1138", Name: "prowlarr-config-clone"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.VolumeName != "pvc-6bce613c" {
		t.Fatalf("the bound claim was modified: volumeName=%q", got.Spec.VolumeName)
	}
}
