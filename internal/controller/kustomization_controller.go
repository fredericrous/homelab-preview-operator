package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
)

const (
	// PreviewEnvironmentLabel marks a Kustomization as a preview environment
	PreviewEnvironmentLabel = "preview-environment"

	// AppNameLabel identifies the app being previewed
	AppNameLabel = "app.kubernetes.io/name"

	// PatchesAppliedAnnotation indicates the operator has applied patches
	PatchesAppliedAnnotation = "preview.homelab.io/patches-applied"
)

// KustomizationReconciler watches Flux Kustomizations with preview labels
type KustomizationReconciler struct {
	client.Client
	Log           logr.Logger
	Scheme        *runtime.Scheme
	PreviewDomain string
}

// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories,verbs=get;list;watch
// +kubebuilder:rbac:groups=security.homelab.io,resources=oidcclients,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=redis.redis.opstreelabs.in,resources=redis,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces;secrets;configmaps;services,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=preview.homelab.io,resources=previewconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create

// Reconcile handles Kustomization events
func (r *KustomizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("kustomization", req.NamespacedName)

	// Fetch the Kustomization
	var ks kustomizev1.Kustomization
	if err := r.Get(ctx, req.NamespacedName, &ks); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Check if this is a preview environment
	if ks.Labels[PreviewEnvironmentLabel] != "true" {
		return ctrl.Result{}, nil
	}

	// Get the app name from labels
	appName := ks.Labels[AppNameLabel]
	if appName == "" {
		appName = inferAppNameFromNamespace(ks.Namespace)
	}

	if appName == "" {
		log.Info("Could not determine app name, skipping")
		return ctrl.Result{}, nil
	}

	handler := NewPreviewHandler(r.Client, log, r.PreviewDomain)

	// State 1: Kustomization is suspended → prepare patches and unsuspend
	if ks.Spec.Suspend {
		log.Info("Kustomization is suspended, preparing patches", "app", appName)
		if err := handler.PrepareKustomization(ctx, &ks, appName); err != nil {
			log.Error(err, "Failed to prepare Kustomization patches")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}
		log.Info("Patches applied and Kustomization unsuspended", "app", appName)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// State 2: Patches applied, Kustomization running → create infrastructure
	annotations := ks.GetAnnotations()
	if annotations != nil && annotations[PatchesAppliedAnnotation] == "true" {
		log.Info("Creating preview infrastructure", "app", appName)
		if err := handler.CreateInfrastructure(ctx, &ks, appName); err != nil {
			log.Error(err, "Failed to create preview infrastructure")
			return ctrl.Result{RequeueAfter: 60 * time.Second}, err
		}
		log.Info("Preview infrastructure ready", "app", appName)
		return ctrl.Result{}, nil
	}

	// Backward compatibility: not suspended, no patches-applied annotation → old flow
	log.Info("Processing preview Kustomization (legacy flow)", "app", appName)
	if err := handler.Process(ctx, &ks, appName); err != nil {
		log.Error(err, "Failed to process preview environment")
		return ctrl.Result{}, err
	}

	log.Info("Successfully processed preview environment", "app", appName)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *KustomizationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	previewLabelPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()[PreviewEnvironmentLabel] == "true"
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&kustomizev1.Kustomization{}).
		WithEventFilter(previewLabelPredicate).
		Owns(&corev1.Secret{}).
		Complete(r)
}

// inferAppNameFromNamespace tries to extract app name from preview namespace
func inferAppNameFromNamespace(namespace string) string {
	return ""
}
