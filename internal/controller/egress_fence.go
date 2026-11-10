package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;create;update;delete

const egressFenceName = "preview-egress-fence"

// egressFence is the NetworkPolicy that confines a preview's pods to kube-dns and their own
// namespace. NFS is mounted by the kubelet on the node, so the fence does not cut the share.
func egressFence(namespace string) *networkingv1.NetworkPolicy {
	dnsPort := intstr.FromInt32(53)
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: egressFenceName, Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &dnsPort},
						{Protocol: &tcp, Port: &dnsPort},
					},
				},
				{
					To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
				},
			},
		},
	}
}

// ensureEgressFence creates or updates the fence in the preview namespace.
func (h *PreviewHandler) ensureEgressFence(ctx context.Context, namespace string) error {
	if err := h.createOrUpdateTyped(ctx, egressFence(namespace)); err != nil {
		return fmt.Errorf("ensure NetworkPolicy %q in namespace %q: %w", egressFenceName, namespace, err)
	}
	return nil
}

// removeEgressFence deletes a fence left from a config that no longer isolates egress.
func (h *PreviewHandler) removeEgressFence(ctx context.Context, namespace string) error {
	stale := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: egressFenceName, Namespace: namespace}}
	if err := h.client.Delete(ctx, stale); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete NetworkPolicy %q in namespace %q: %w", egressFenceName, namespace, err)
	}
	return nil
}
