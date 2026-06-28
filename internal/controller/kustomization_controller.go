package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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

	// InfraCreatedAnnotation indicates infrastructure resources (CNPG, Redis, etc.) have been created
	InfraCreatedAnnotation = "preview.homelab.io/infra-created"

	// CredentialsPatchedAnnotation indicates DB credentials have been injected into patches
	CredentialsPatchedAnnotation = "preview.homelab.io/credentials-patched"

	// PRCommentedAnnotation indicates a PR comment with the preview URL has been posted
	PRCommentedAnnotation = "preview.homelab.io/pr-commented"

	// ConfigHashAnnotation records the PreviewConfig spec hash the patches were
	// rendered from. A changed PreviewConfig (different hash) triggers a re-render
	// so in-flight previews pick up config edits (e.g. an added removeContainers).
	ConfigHashAnnotation = "preview.homelab.io/config-hash"

	// PreviewFinalizer ensures the cluster-scoped VolumeSnapshotContents (Retain)
	// and prod-namespace VolumeSnapshots created for clones are deleted on teardown
	// — namespace deletion can't GC them, so they'd leak orphaned Ceph snapshots.
	PreviewFinalizer = "preview.homelab.io/cleanup"
)

// KustomizationReconciler watches Flux Kustomizations with preview labels
type KustomizationReconciler struct {
	client.Client
	Log           logr.Logger
	Scheme        *runtime.Scheme
	PreviewDomain string
	GitHubRepo    string
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
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotcontents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

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
	prNumber := strings.TrimPrefix(ks.Namespace, "preview-pr-")

	// Teardown: on deletion, clean up the cluster-scoped VSCs (Retain) and the
	// prod-namespace VolumeSnapshots the clone created — namespace deletion can't
	// GC them, so without this they leak orphaned Ceph snapshots.
	if !ks.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&ks, PreviewFinalizer) {
			if err := handler.TeardownSnapshots(ctx, appName, prNumber); err != nil {
				log.Error(err, "snapshot teardown failed; will retry")
				return ctrl.Result{RequeueAfter: 15 * time.Second}, err
			}
			controllerutil.RemoveFinalizer(&ks, PreviewFinalizer)
			if err := r.Update(ctx, &ks); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure the cleanup finalizer is present before we create any snapshots.
	if controllerutil.AddFinalizer(&ks, PreviewFinalizer) {
		if err := r.Update(ctx, &ks); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

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

	annotations := ks.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	// New flow: 3-state progression after patches are applied
	if annotations[PatchesAppliedAnnotation] == "true" {
		// Re-render if the PreviewConfig changed since we last prepared (e.g. an
		// added removeContainers / extraEnv). Reset only the patch + credential
		// state; keep infra-created so we DON'T re-clone, then re-prepare.
		if hash, err := handler.PreviewConfigHash(ctx, ks.Namespace, appName); err == nil && hash != "" && annotations[ConfigHashAnnotation] != hash {
			log.Info("PreviewConfig changed, re-rendering patches", "app", appName)
			delete(annotations, PatchesAppliedAnnotation)
			delete(annotations, CredentialsPatchedAnnotation)
			ks.SetAnnotations(annotations)
			ks.Spec.Suspend = true
			if err := r.Update(ctx, &ks); err != nil {
				return ctrl.Result{RequeueAfter: 10 * time.Second}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// State 2: Create infrastructure (CNPG, Redis, S3, etc.)
		if annotations[InfraCreatedAnnotation] != "true" {
			log.Info("Creating preview infrastructure", "app", appName)
			if err := handler.CreateInfrastructure(ctx, &ks, appName); err != nil {
				log.Error(err, "Failed to create preview infrastructure")
				return ctrl.Result{RequeueAfter: 60 * time.Second}, err
			}
			annotations[InfraCreatedAnnotation] = "true"
			ks.SetAnnotations(annotations)
			if err := r.Update(ctx, &ks); err != nil {
				return ctrl.Result{RequeueAfter: 10 * time.Second}, err
			}
			log.Info("Preview infrastructure created", "app", appName)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}

		// State 3: Update patches with DB credentials once CNPG is ready
		if annotations[CredentialsPatchedAnnotation] != "true" {
			if err := handler.UpdateDBCredentialPatches(ctx, &ks, appName); err != nil {
				log.Info("Waiting for DB credentials to become available", "error", err)
				return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
			}
			log.Info("Preview environment fully ready", "app", appName)
			return ctrl.Result{}, nil
		}

		// Post a PR comment with the preview URL (once, non-blocking)
		if annotations[PRCommentedAnnotation] != "true" && r.GitHubRepo != "" {
			prNumber := strings.TrimPrefix(ks.Namespace, "preview-pr-")
			previewURL := fmt.Sprintf("https://pr-%s-%s.%s", prNumber, appName, r.PreviewDomain)
			if err := PostPRComment(ctx, r.Client, ks.Namespace, r.GitHubRepo, prNumber, appName, previewURL, log); err != nil {
				log.Error(err, "Failed to post PR comment", "pr", prNumber)
			} else {
				annotations[PRCommentedAnnotation] = "true"
				ks.SetAnnotations(annotations)
				if err := r.Update(ctx, &ks); err != nil {
					log.Error(err, "Failed to save PR comment annotation")
				}
			}
		}

		// All states complete — verify patches are still present (ResourceSet may overwrite)
		if len(ks.Spec.Patches) == 0 {
			log.Info("Patches were cleared (likely by ResourceSet), re-applying", "app", appName)
			delete(annotations, CredentialsPatchedAnnotation)
			delete(annotations, PatchesAppliedAnnotation)
			ks.SetAnnotations(annotations)
			ks.Spec.Suspend = true
			if err := r.Update(ctx, &ks); err != nil {
				return ctrl.Result{RequeueAfter: 10 * time.Second}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// Ensure HelmRelease has correct preview values (kustomize merge may not
		// deep-merge spec.values due to x-kubernetes-preserve-unknown-fields)
		patched, err := handler.ensureHelmReleaseValues(ctx, ks.Namespace, appName)
		if err != nil {
			log.Info("Failed to ensure HelmRelease values", "error", err)
		}
		if patched {
			log.Info("Re-applied HelmRelease preview values", "app", appName)
			return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
		}
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
