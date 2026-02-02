package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"

	v1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

// PreviewHandler handles the setup of preview environment resources
type PreviewHandler struct {
	client        client.Client
	log           logr.Logger
	previewDomain string
}

// NewPreviewHandler creates a new PreviewHandler
func NewPreviewHandler(c client.Client, log logr.Logger, domain string) *PreviewHandler {
	return &PreviewHandler{
		client:        c,
		log:           log,
		previewDomain: domain,
	}
}

// Process sets up all preview resources for the given Kustomization
func (h *PreviewHandler) Process(ctx context.Context, ks *kustomizev1.Kustomization, appName string) error {
	namespace := ks.Namespace

	// 1. Fetch preview.yaml from git source
	config, err := h.fetchPreviewConfig(ctx, ks, appName)
	if err != nil {
		if errors.IsNotFound(err) {
			h.log.Info("No preview.yaml found, skipping preview setup", "app", appName)
			return nil
		}
		return fmt.Errorf("failed to fetch preview config: %w", err)
	}

	// 2. Discover and clone OIDC client from production
	if err := h.setupOIDC(ctx, namespace, appName); err != nil {
		return fmt.Errorf("failed to setup OIDC: %w", err)
	}

	// 3. Check and setup database from production namespace annotations
	if err := h.setupDatabase(ctx, namespace, appName); err != nil {
		return fmt.Errorf("failed to setup database: %w", err)
	}

	// 4. Setup Redis if needed
	if config.Spec.Redis != nil && config.Spec.Redis.Enabled {
		if err := h.setupRedis(ctx, namespace); err != nil {
			return fmt.Errorf("failed to setup Redis: %w", err)
		}
	}

	// 5. Setup S3 proxy if needed
	if config.Spec.Storage != nil && config.Spec.Storage.Enabled {
		if err := h.setupS3Proxy(ctx, namespace, config.Spec.Storage.Bucket); err != nil {
			return fmt.Errorf("failed to setup S3 proxy: %w", err)
		}
	}

	// 6. Patch deployment/helmrelease with correct env vars
	if err := h.patchWorkload(ctx, namespace, appName, config); err != nil {
		return fmt.Errorf("failed to patch workload: %w", err)
	}

	return nil
}

// fetchPreviewConfig reads preview.yaml from the git source
func (h *PreviewHandler) fetchPreviewConfig(ctx context.Context, ks *kustomizev1.Kustomization, appName string) (*v1.PreviewConfig, error) {
	// Get the GitRepository referenced by the Kustomization
	gitRepoName := types.NamespacedName{
		Namespace: ks.Spec.SourceRef.Namespace,
		Name:      ks.Spec.SourceRef.Name,
	}
	if gitRepoName.Namespace == "" {
		gitRepoName.Namespace = ks.Namespace
	}

	var gitRepo sourcev1.GitRepository
	if err := h.client.Get(ctx, gitRepoName, &gitRepo); err != nil {
		return nil, fmt.Errorf("failed to get GitRepository: %w", err)
	}

	// The preview.yaml should be fetched from the git artifact
	// For now, we'll read it from a ConfigMap that Flux could populate
	// TODO: Implement direct git archive fetching from source-controller artifact

	// Alternative: Read from ConfigMap if it exists
	configMapName := types.NamespacedName{
		Namespace: ks.Namespace,
		Name:      fmt.Sprintf("%s-preview-config", appName),
	}

	var cm corev1.ConfigMap
	if err := h.client.Get(ctx, configMapName, &cm); err != nil {
		// Try reading from a well-known location
		return h.readPreviewConfigFromNamespace(ctx, appName)
	}

	configData, ok := cm.Data["preview.yaml"]
	if !ok {
		return nil, errors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, configMapName.Name)
	}

	var config v1.PreviewConfig
	if err := yaml.Unmarshal([]byte(configData), &config); err != nil {
		return nil, fmt.Errorf("failed to parse preview.yaml: %w", err)
	}

	return &config, nil
}

// readPreviewConfigFromNamespace tries to read preview config from production namespace
func (h *PreviewHandler) readPreviewConfigFromNamespace(ctx context.Context, appName string) (*v1.PreviewConfig, error) {
	configMapName := types.NamespacedName{
		Namespace: appName, // Production namespace
		Name:      "preview-config",
	}

	var cm corev1.ConfigMap
	if err := h.client.Get(ctx, configMapName, &cm); err != nil {
		return nil, err
	}

	configData, ok := cm.Data["preview.yaml"]
	if !ok {
		return nil, errors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, configMapName.Name)
	}

	var config v1.PreviewConfig
	if err := yaml.Unmarshal([]byte(configData), &config); err != nil {
		return nil, fmt.Errorf("failed to parse preview.yaml: %w", err)
	}

	return &config, nil
}

// setupOIDC discovers and clones the OIDC client from production
func (h *PreviewHandler) setupOIDC(ctx context.Context, namespace, appName string) error {
	// Look for OIDCClient in production namespace
	oidcClient := &unstructured.Unstructured{}
	oidcClient.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "security.homelab.io",
		Version: "v1alpha1",
		Kind:    "OIDCClient",
	})

	prodOIDCName := types.NamespacedName{
		Namespace: appName,
		Name:      appName, // Convention: OIDCClient name matches app name
	}

	if err := h.client.Get(ctx, prodOIDCName, oidcClient); err != nil {
		if errors.IsNotFound(err) {
			h.log.Info("No OIDCClient found in production, skipping OIDC setup", "app", appName)
			return nil
		}
		return err
	}

	// Extract redirect URI and modify for preview
	spec, found, err := unstructured.NestedMap(oidcClient.Object, "spec")
	if err != nil || !found {
		return fmt.Errorf("failed to get OIDCClient spec: %w", err)
	}

	// Get existing redirect URIs to extract callback path
	redirectURIs, _, _ := unstructured.NestedStringSlice(spec, "redirectUris")
	callbackPath := "/oauth/callback" // Default
	if len(redirectURIs) > 0 {
		// Extract path from first redirect URI
		parts := strings.SplitN(redirectURIs[0], "/", 4)
		if len(parts) >= 4 {
			callbackPath = "/" + parts[3]
		}
	}

	// Extract PR number from namespace (preview-pr-{id})
	prNumber := strings.TrimPrefix(namespace, "preview-pr-")

	// Create preview OIDCClient
	previewOIDC := &unstructured.Unstructured{}
	previewOIDC.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "security.homelab.io",
		Version: "v1alpha1",
		Kind:    "OIDCClient",
	})
	previewOIDC.SetNamespace(namespace)
	previewOIDC.SetName(fmt.Sprintf("preview-%s-%s", appName, prNumber))

	// Copy spec and modify
	newSpec := make(map[string]interface{})
	for k, v := range spec {
		newSpec[k] = v
	}

	// Update client ID and redirect URI
	newSpec["clientId"] = fmt.Sprintf("preview-%s-%s", appName, prNumber)
	newSpec["clientName"] = fmt.Sprintf("%s Preview PR #%s", appName, prNumber)
	newSpec["redirectUris"] = []interface{}{
		fmt.Sprintf("https://pr-%s-%s.%s%s", prNumber, appName, h.previewDomain, callbackPath),
	}

	if err := unstructured.SetNestedMap(previewOIDC.Object, newSpec, "spec"); err != nil {
		return fmt.Errorf("failed to set OIDCClient spec: %w", err)
	}

	// Create or update the OIDCClient
	if err := h.client.Create(ctx, previewOIDC); err != nil {
		if errors.IsAlreadyExists(err) {
			// Update existing
			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(previewOIDC.GroupVersionKind())
			if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: previewOIDC.GetName()}, existing); err != nil {
				return err
			}
			previewOIDC.SetResourceVersion(existing.GetResourceVersion())
			if err := h.client.Update(ctx, previewOIDC); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	h.log.Info("Created preview OIDCClient", "name", previewOIDC.GetName())
	return nil
}

// setupDatabase checks production namespace for postgres annotations and sets up CNPG
func (h *PreviewHandler) setupDatabase(ctx context.Context, namespace, appName string) error {
	// Check production namespace for postgres.cnpg.io/cluster-name annotation
	var prodNS corev1.Namespace
	if err := h.client.Get(ctx, types.NamespacedName{Name: appName}, &prodNS); err != nil {
		if errors.IsNotFound(err) {
			h.log.Info("Production namespace not found, skipping database setup", "app", appName)
			return nil
		}
		return err
	}

	clusterName := prodNS.Annotations["postgres.cnpg.io/cluster-name"]
	if clusterName == "" {
		h.log.Info("No postgres annotation found, skipping database setup", "app", appName)
		return nil
	}

	h.log.Info("App uses PostgreSQL, setting up CNPG cluster from snapshot", "cluster", clusterName)

	// The database setup involves:
	// 1. Creating VolumeSnapshot from production replica
	// 2. Creating CNPG Cluster that bootstraps from snapshot
	// This is complex and should be delegated to a Job or done inline
	// For now, we'll create the necessary resources

	// TODO: Implement CNPG cluster creation from VolumeSnapshot
	// This mirrors the existing db-setup Job logic

	return nil
}

// setupRedis creates a Redis instance for the preview
func (h *PreviewHandler) setupRedis(ctx context.Context, namespace string) error {
	redis := &unstructured.Unstructured{}
	redis.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "redis.redis.opstreelabs.in",
		Version: "v1beta2",
		Kind:    "Redis",
	})
	redis.SetNamespace(namespace)
	redis.SetName("redis-preview")

	spec := map[string]interface{}{
		"kubernetesConfig": map[string]interface{}{
			"image":           "quay.io/opstree/redis:v7.0.15",
			"imagePullPolicy": "IfNotPresent",
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{
					"cpu":    "50m",
					"memory": "64Mi",
				},
				"limits": map[string]interface{}{
					"cpu":    "200m",
					"memory": "128Mi",
				},
			},
		},
		"storage": map[string]interface{}{
			"volumeClaimTemplate": map[string]interface{}{
				"spec": map[string]interface{}{
					"storageClassName": "rook-ceph-block",
					"accessModes":      []interface{}{"ReadWriteOnce"},
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{
							"storage": "1Gi",
						},
					},
				},
			},
		},
		"podSecurityContext": map[string]interface{}{
			"runAsUser":    int64(1000),
			"fsGroup":      int64(1000),
			"runAsNonRoot": true,
		},
	}

	if err := unstructured.SetNestedMap(redis.Object, spec, "spec"); err != nil {
		return err
	}

	if err := h.client.Create(ctx, redis); err != nil {
		if !errors.IsAlreadyExists(err) {
			return err
		}
	}

	h.log.Info("Created Redis instance")
	return nil
}

// setupS3Proxy creates an S3 proxy deployment for storage isolation
func (h *PreviewHandler) setupS3Proxy(ctx context.Context, namespace, bucket string) error {
	// TODO: Implement S3 proxy deployment
	// This mirrors the existing s3proxy setup in ResourceSet
	h.log.Info("S3 proxy setup not yet implemented", "bucket", bucket)
	return nil
}

// patchWorkload patches the Deployment or HelmRelease with preview values
func (h *PreviewHandler) patchWorkload(ctx context.Context, namespace, appName string, config *v1.PreviewConfig) error {
	prNumber := strings.TrimPrefix(namespace, "preview-pr-")

	switch config.Spec.DeploymentType {
	case v1.DeploymentTypeHelm:
		return h.patchHelmRelease(ctx, namespace, appName, prNumber, config)
	default:
		return h.patchDeployment(ctx, namespace, appName, prNumber, config)
	}
}

// patchDeployment patches a Deployment with preview env vars
func (h *PreviewHandler) patchDeployment(ctx context.Context, namespace, appName, prNumber string, config *v1.PreviewConfig) error {
	// TODO: Implement deployment patching
	// This will use the envMapping from config to set correct env vars
	h.log.Info("Deployment patching not yet implemented", "app", appName)
	return nil
}

// patchHelmRelease patches a HelmRelease with preview values
func (h *PreviewHandler) patchHelmRelease(ctx context.Context, namespace, appName, prNumber string, config *v1.PreviewConfig) error {
	// TODO: Implement HelmRelease patching
	// This will use the helmValues from config to set correct values
	h.log.Info("HelmRelease patching not yet implemented", "app", appName)
	return nil
}
