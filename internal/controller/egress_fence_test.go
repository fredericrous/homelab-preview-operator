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

func fencedConfig(isolate bool) *v1.PreviewConfig {
	return &v1.PreviewConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "radarr", Namespace: "radarr"},
		Spec:       v1.PreviewConfigSpec{IsolateEgress: isolate},
	}
}

func getFence(t *testing.T, h *PreviewHandler) (*networkingv1.NetworkPolicy, error) {
	t.Helper()
	var fence networkingv1.NetworkPolicy
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "preview-pr-7", Name: "preview-egress-fence"}, &fence)
	return &fence, err
}

func TestEgressFenceSpecIsExact(t *testing.T) {
	h := fenceTestHandler(t, interceptor.Funcs{}, fencedConfig(true))
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
	h = fenceTestHandler(t, funcs, fencedConfig(true))
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
	h := fenceTestHandler(t, funcs, fencedConfig(true))
	ks := storedKustomization(t, h)
	err := h.PrepareKustomization(context.Background(), ks, "radarr")
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the create error", err)
	}
	if !ks.Spec.Suspend || len(ks.Spec.Patches) != 0 {
		t.Fatalf("Kustomization must stay suspended with no patches: suspend=%v patches=%d", ks.Spec.Suspend, len(ks.Spec.Patches))
	}
}

func TestEgressFenceSecondRunTakesAlreadyExistsPath(t *testing.T) {
	var creates, updates int
	funcs := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
				creates++
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
				updates++
			}
			return c.Update(ctx, obj, opts...)
		},
	}
	h := fenceTestHandler(t, funcs, fencedConfig(true))
	for run := 0; run < 2; run++ {
		if err := h.PrepareKustomization(context.Background(), storedKustomization(t, h), "radarr"); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	if creates != 2 || updates != 1 {
		t.Fatalf("creates=%d updates=%d, want 2 creates (second AlreadyExists) and 1 update", creates, updates)
	}
	if _, err := getFence(t, h); err != nil {
		t.Fatal(err)
	}
}

func TestEgressFenceDeletedWhenFlagGoesFalse(t *testing.T) {
	h := fenceTestHandler(t, interceptor.Funcs{}, fencedConfig(true))
	if err := h.PrepareKustomization(context.Background(), storedKustomization(t, h), "radarr"); err != nil {
		t.Fatal(err)
	}
	var config v1.PreviewConfig
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "radarr", Name: "radarr"}, &config); err != nil {
		t.Fatal(err)
	}
	config.Spec.IsolateEgress = false
	if err := h.client.Update(context.Background(), &config); err != nil {
		t.Fatal(err)
	}
	if err := h.PrepareKustomization(context.Background(), storedKustomization(t, h), "radarr"); err != nil {
		t.Fatal(err)
	}
	if _, err := getFence(t, h); !apierrors.IsNotFound(err) {
		t.Fatalf("fence must be deleted, Get error = %v", err)
	}
}

func intstrPort53() intstr.IntOrString { return intstr.FromInt32(53) }
