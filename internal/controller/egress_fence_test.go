package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	logr "github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

func newSuspendedKustomization() *kustomizev1.Kustomization {
	return &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{Namespace: "preview-pr-7", Name: "radarr"},
		Spec:       kustomizev1.KustomizationSpec{Suspend: true},
	}
}

func fenceTestHandler(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *PreviewHandler {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme, kustomizev1.AddToScheme, v1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, newSuspendedKustomization())...).WithInterceptorFuncs(funcs).Build()
	return &PreviewHandler{client: c, log: logr.Discard(), previewDomain: "example.test"}
}

func storedKustomization(t *testing.T, h *PreviewHandler) *kustomizev1.Kustomization {
	t.Helper()
	var ks kustomizev1.Kustomization
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "preview-pr-7", Name: "radarr"}, &ks); err != nil {
		t.Fatal(err)
	}
	return &ks
}

func isolatedPreviewConfig() *v1.PreviewConfig {
	return &v1.PreviewConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "radarr", Namespace: "radarr"},
		Spec:       v1.PreviewConfigSpec{IsolateEgress: true},
	}
}

// turnIsolationOff flips the stored PreviewConfig the way an adopter's edit would.
func turnIsolationOff(t *testing.T, h *PreviewHandler) {
	t.Helper()
	var config v1.PreviewConfig
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "radarr", Name: "radarr"}, &config); err != nil {
		t.Fatal(err)
	}
	config.Spec.IsolateEgress = false
	if err := h.client.Update(context.Background(), &config); err != nil {
		t.Fatal(err)
	}
}

func getFence(t *testing.T, h *PreviewHandler) (*networkingv1.NetworkPolicy, error) {
	t.Helper()
	var fence networkingv1.NetworkPolicy
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "preview-pr-7", Name: "preview-egress-fence"}, &fence)
	return &fence, err
}

func TestEgressFenceSpecIsExact(t *testing.T) {
	h := fenceTestHandler(t, interceptor.Funcs{}, isolatedPreviewConfig())
	ks := storedKustomization(t, h)
	if err := h.PrepareKustomization(context.Background(), ks, "radarr"); err != nil {
		t.Fatal(err)
	}
	fence, err := getFence(t, h)
	if err != nil {
		t.Fatal(err)
	}
	dns := intstrPort53()
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	want := networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		Egress: []networkingv1.NetworkPolicyEgressRule{
			{
				To: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
			},
			{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}},
		},
	}
	if !reflect.DeepEqual(fence.Spec, want) {
		t.Fatalf("fence spec = %+v\nwant %+v", fence.Spec, want)
	}
	if fence.Labels["app.kubernetes.io/managed-by"] != "homelab-preview-operator" {
		t.Fatalf("fence labels = %v", fence.Labels)
	}
	if ks.Spec.Suspend {
		t.Fatal("the Kustomization must be unsuspended once the fence exists")
	}
}

func TestEgressFenceExistsBeforeUnsuspend(t *testing.T) {
	var fenceExistedAtUpdate bool
	var h *PreviewHandler
	funcs := interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if _, ok := obj.(*kustomizev1.Kustomization); ok {
			_, err := getFence(t, h)
			fenceExistedAtUpdate = err == nil
		}
		return c.Update(ctx, obj, opts...)
	}}
	h = fenceTestHandler(t, funcs, isolatedPreviewConfig())
	ks := storedKustomization(t, h)
	if err := h.PrepareKustomization(context.Background(), ks, "radarr"); err != nil {
		t.Fatal(err)
	}
	if !fenceExistedAtUpdate {
		t.Fatal("the fence must exist before the Kustomization is updated unsuspended")
	}
}

func TestEgressFenceCreateFailureFailsClosed(t *testing.T) {
	boom := errors.New("apiserver down")
	funcs := interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
			return boom
		}
		return c.Create(ctx, obj, opts...)
	}}
	h := fenceTestHandler(t, funcs, isolatedPreviewConfig())
	ks := storedKustomization(t, h)
	err := h.PrepareKustomization(context.Background(), ks, "radarr")
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the create error", err)
	}
	if !ks.Spec.Suspend || len(ks.Spec.Patches) != 0 {
		t.Fatalf("Kustomization must stay suspended with no patches: suspend=%v patches=%d", ks.Spec.Suspend, len(ks.Spec.Patches))
	}
}

func TestEgressFenceSecondRunRestoresTamperedSpec(t *testing.T) {
	h := fenceTestHandler(t, interceptor.Funcs{}, isolatedPreviewConfig())
	if err := h.PrepareKustomization(context.Background(), storedKustomization(t, h), "radarr"); err != nil {
		t.Fatal(err)
	}
	original, err := getFence(t, h)
	if err != nil {
		t.Fatal(err)
	}
	wantSpec := *original.Spec.DeepCopy()
	original.Spec.Egress = original.Spec.Egress[1:] // drop the DNS rule
	if err := h.client.Update(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	if err := h.PrepareKustomization(context.Background(), storedKustomization(t, h), "radarr"); err != nil {
		t.Fatal(err)
	}
	restored, err := getFence(t, h)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Spec, wantSpec) {
		t.Fatalf("second run must restore the fence spec: %+v", restored.Spec)
	}
}

func TestEgressFenceDeletedWhenFlagGoesFalse(t *testing.T) {
	h := fenceTestHandler(t, interceptor.Funcs{}, isolatedPreviewConfig())
	if err := h.PrepareKustomization(context.Background(), storedKustomization(t, h), "radarr"); err != nil {
		t.Fatal(err)
	}
	turnIsolationOff(t, h)
	if err := h.PrepareKustomization(context.Background(), storedKustomization(t, h), "radarr"); err != nil {
		t.Fatal(err)
	}
	if _, err := getFence(t, h); !apierrors.IsNotFound(err) {
		t.Fatalf("fence must be deleted, Get error = %v", err)
	}
}

func TestEgressFenceDeleteFailureFailsClosed(t *testing.T) {
	boom := errors.New("apiserver down")
	funcs := interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
			return boom
		}
		return c.Delete(ctx, obj, opts...)
	}}
	h := fenceTestHandler(t, funcs, isolatedPreviewConfig())
	if err := h.PrepareKustomization(context.Background(), storedKustomization(t, h), "radarr"); err != nil {
		t.Fatal(err)
	}
	turnIsolationOff(t, h)
	ks := storedKustomization(t, h)
	ks.Spec.Suspend = true
	err := h.PrepareKustomization(context.Background(), ks, "radarr")
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the delete error", err)
	}
	if !ks.Spec.Suspend {
		t.Fatal("a failed fence removal must leave the Kustomization suspended")
	}
}

func TestEgressFenceRemovalLeavesUnmanagedPolicy(t *testing.T) {
	adopters := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "preview-egress-fence", Namespace: "preview-pr-7"}}
	notIsolated := isolatedPreviewConfig()
	notIsolated.Spec.IsolateEgress = false
	h := fenceTestHandler(t, interceptor.Funcs{}, notIsolated, adopters)
	if err := h.PrepareKustomization(context.Background(), storedKustomization(t, h), "radarr"); err != nil {
		t.Fatal(err)
	}
	if _, err := getFence(t, h); err != nil {
		t.Fatalf("a policy without the operator's label must be left alone: %v", err)
	}
}

func intstrPort53() intstr.IntOrString { return intstr.FromInt32(53) }
