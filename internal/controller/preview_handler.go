package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"

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
	prNumber := strings.TrimPrefix(namespace, "preview-pr-")

	// 1. Fetch preview.yaml from ConfigMap (populated by ResourceSet or init container)
	config, err := h.fetchPreviewConfig(ctx, namespace, appName)
	if err != nil {
		if errors.IsNotFound(err) {
			h.log.Info("No preview.yaml found, skipping preview setup", "app", appName)
			return nil
		}
		return fmt.Errorf("failed to fetch preview config: %w", err)
	}

	// 2. Discover and clone OIDC client from production
	if err := h.setupOIDC(ctx, namespace, appName, prNumber); err != nil {
		return fmt.Errorf("failed to setup OIDC: %w", err)
	}

	// 3. Check and setup database from production namespace annotations
	if err := h.setupDatabase(ctx, namespace, appName, prNumber); err != nil {
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
		if err := h.setupS3Proxy(ctx, namespace, config.Spec.Storage.Bucket, prNumber); err != nil {
			return fmt.Errorf("failed to setup S3 proxy: %w", err)
		}
	}

	// 6. Setup config volume clone with URL rewriting
	if config.Spec.ConfigVolume != nil {
		if err := h.setupConfigVolume(ctx, namespace, appName, prNumber, config); err != nil {
			return fmt.Errorf("failed to setup config volume: %w", err)
		}
	}

	// 7. Patch deployment/helmrelease with correct env vars
	if err := h.patchWorkload(ctx, namespace, appName, prNumber, config); err != nil {
		return fmt.Errorf("failed to patch workload: %w", err)
	}

	return nil
}

// PrepareKustomization generates patches for a suspended Kustomization and unsuspends it.
// This is the new flow: ResourceSet creates a suspended Kustomization, operator fills patches.
func (h *PreviewHandler) PrepareKustomization(ctx context.Context, ks *kustomizev1.Kustomization, appName string) error {
	namespace := ks.Namespace
	prNumber := strings.TrimPrefix(namespace, "preview-pr-")

	// Fetch PreviewConfig from production namespace
	config, err := h.fetchPreviewConfig(ctx, namespace, appName)
	if err != nil {
		if errors.IsNotFound(err) {
			h.log.Info("No PreviewConfig found, unsuspending with strip patches only", "app", appName)
			// Still apply strip patches even without app config
			config = &v1.PreviewConfig{}
		} else {
			return fmt.Errorf("failed to fetch preview config: %w", err)
		}
	}

	// Discover OIDC callback path from production
	callbackPath := h.discoverOIDCCallbackPath(ctx, appName)

	// Generate patches without DB credentials — CNPG hasn't been created yet.
	// DB credentials will be injected later by UpdateDBCredentialPatches (State 3).
	allPatches := buildAllPatches(appName, prNumber, namespace, h.previewDomain, config, callbackPath, nil)

	// Apply patches to the Kustomization
	ks.Spec.Patches = allPatches

	// Set annotation to mark patches as applied
	annotations := ks.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[PatchesAppliedAnnotation] = "true"
	if hash := hashPreviewConfigSpec(config); hash != "" {
		annotations[ConfigHashAnnotation] = hash
	}
	ks.SetAnnotations(annotations)

	// Unsuspend the Kustomization
	ks.Spec.Suspend = false

	// Update the Kustomization
	if err := h.client.Update(ctx, ks); err != nil {
		return fmt.Errorf("failed to update Kustomization with patches: %w", err)
	}

	h.log.Info("Prepared Kustomization with patches", "app", appName, "patchCount", len(allPatches))
	return nil
}

// CreateInfrastructure sets up all preview infrastructure resources.
// Called after the Kustomization has been patched and is reconciling.
func (h *PreviewHandler) CreateInfrastructure(ctx context.Context, ks *kustomizev1.Kustomization, appName string) error {
	namespace := ks.Namespace
	prNumber := strings.TrimPrefix(namespace, "preview-pr-")

	config, err := h.fetchPreviewConfig(ctx, namespace, appName)
	if err != nil {
		if errors.IsNotFound(err) {
			h.log.Info("No PreviewConfig found, skipping infrastructure setup", "app", appName)
			return nil
		}
		return fmt.Errorf("failed to fetch preview config: %w", err)
	}

	// Setup OIDC with operator-managed secret
	if err := h.setupOIDC(ctx, namespace, appName, prNumber); err != nil {
		return fmt.Errorf("failed to setup OIDC: %w", err)
	}

	// Setup database from production snapshot
	if err := h.setupDatabase(ctx, namespace, appName, prNumber); err != nil {
		return fmt.Errorf("failed to setup database: %w", err)
	}

	// Setup Redis if needed
	if config.Spec.Redis != nil && config.Spec.Redis.Enabled {
		if err := h.setupRedis(ctx, namespace); err != nil {
			return fmt.Errorf("failed to setup Redis: %w", err)
		}
	}

	// Setup S3 proxy if needed
	if config.Spec.Storage != nil && config.Spec.Storage.Enabled {
		if err := h.setupS3Proxy(ctx, namespace, config.Spec.Storage.Bucket, prNumber); err != nil {
			return fmt.Errorf("failed to setup S3 proxy: %w", err)
		}
	}

	// Setup config volume clone with URL rewriting
	if config.Spec.ConfigVolume != nil {
		if err := h.setupConfigVolume(ctx, namespace, appName, prNumber, config); err != nil {
			return fmt.Errorf("failed to setup config volume: %w", err)
		}
	}

	// Note: patchWorkload is NOT called here - Kustomization patches handle env/helm overrides

	return nil
}

// UpdateDBCredentialPatches waits for the CNPG superuser secret and regenerates
// Kustomization patches with actual DB credentials. Called as State 3, after
// CreateInfrastructure has created the CNPG cluster.
func (h *PreviewHandler) UpdateDBCredentialPatches(ctx context.Context, ks *kustomizev1.Kustomization, appName string) error {
	namespace := ks.Namespace
	prNumber := strings.TrimPrefix(namespace, "preview-pr-")

	config, err := h.fetchPreviewConfig(ctx, namespace, appName)
	if err != nil {
		if errors.IsNotFound(err) {
			return h.setAnnotation(ctx, ks, CredentialsPatchedAnnotation, "true")
		}
		return err
	}

	// Check if the app needs DB credentials in Helm patches
	needsDBCreds := config.Spec.HelmValues != nil &&
		(config.Spec.HelmValues.DatabaseUser != "" || config.Spec.HelmValues.DatabasePassword != "")

	if !needsDBCreds {
		return h.setAnnotation(ctx, ks, CredentialsPatchedAnnotation, "true")
	}

	// Read the CNPG superuser secret
	secretName := fmt.Sprintf("postgres-preview-%s-superuser", prNumber)
	secret := &corev1.Secret{}
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, secret); err != nil {
		return fmt.Errorf("CNPG superuser secret %s not yet available: %w", secretName, err)
	}

	dbCreds := &DBCredentials{
		Username: string(secret.Data["username"]),
		Password: string(secret.Data["password"]),
	}

	// Regenerate all patches with DB credentials
	callbackPath := h.discoverOIDCCallbackPath(ctx, appName)
	allPatches := buildAllPatches(appName, prNumber, namespace, h.previewDomain, config, callbackPath, dbCreds)

	// Update the Kustomization with new patches and mark credentials as patched
	ks.Spec.Patches = allPatches
	annotations := ks.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[CredentialsPatchedAnnotation] = "true"
	ks.SetAnnotations(annotations)

	if err := h.client.Update(ctx, ks); err != nil {
		return fmt.Errorf("failed to update Kustomization with DB credentials: %w", err)
	}

	h.log.Info("Updated Kustomization patches with DB credentials", "app", appName)

	// Direct-patch the HelmRelease to work around kustomize strategic merge issues
	// with x-kubernetes-preserve-unknown-fields on spec.values
	if _, err := h.ensureHelmReleaseValues(ctx, namespace, appName); err != nil {
		h.log.Info("Could not directly patch HelmRelease (will retry on next reconcile)", "error", err)
	}

	return nil
}

// setAnnotation is a helper to set a single annotation on a Kustomization and persist it
func (h *PreviewHandler) setAnnotation(ctx context.Context, ks *kustomizev1.Kustomization, key, value string) error {
	annotations := ks.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[key] = value
	ks.SetAnnotations(annotations)
	return h.client.Update(ctx, ks)
}

// discoverOIDCCallbackPath reads the OIDCClient from production to extract the callback path
func (h *PreviewHandler) discoverOIDCCallbackPath(ctx context.Context, appName string) string {
	oidcClient := &unstructured.Unstructured{}
	oidcClient.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "security.homelab.io",
		Version: "v1alpha1",
		Kind:    "OIDCClient",
	})

	if err := h.client.Get(ctx, types.NamespacedName{Namespace: appName, Name: appName}, oidcClient); err != nil {
		return "/oauth/callback"
	}

	redirectURIs, _, _ := unstructured.NestedStringSlice(oidcClient.Object, "spec", "redirectUris")
	if len(redirectURIs) > 0 {
		uri := redirectURIs[0]
		if idx := strings.Index(uri, "//"); idx != -1 {
			remainder := uri[idx+2:]
			if pathIdx := strings.Index(remainder, "/"); pathIdx != -1 {
				return remainder[pathIdx:]
			}
		}
	}
	return "/oauth/callback"
}

// generateRandomString generates a cryptographically random hex string of the given byte length
func generateRandomString(byteLen int) string {
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		// Fallback to a predictable but unique-ish value
		return "preview-fallback-secret"
	}
	return hex.EncodeToString(b)
}

// fetchPreviewConfig fetches the PreviewConfig CRD for the app
// It first checks the preview namespace, then falls back to the production namespace
func (h *PreviewHandler) fetchPreviewConfig(ctx context.Context, namespace, appName string) (*v1.PreviewConfig, error) {
	config := &v1.PreviewConfig{}
	name := types.NamespacedName{Namespace: appName, Name: appName}
	if err := h.client.Get(ctx, name, config); err != nil {
		return nil, err
	}
	return config, nil
}

// setupOIDC discovers and clones the OIDC client from production
func (h *PreviewHandler) setupOIDC(ctx context.Context, namespace, appName, prNumber string) error {
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

	// Extract spec
	spec, found, err := unstructured.NestedMap(oidcClient.Object, "spec")
	if err != nil || !found {
		return fmt.Errorf("failed to get OIDCClient spec: %w", err)
	}

	// Get existing redirect URIs to extract callback path
	redirectURIs, _, _ := unstructured.NestedStringSlice(spec, "redirectUris")
	callbackPath := "/oauth/callback" // Default
	if len(redirectURIs) > 0 {
		// Extract path from first redirect URI (e.g., https://app.domain.com/oauth/callback)
		uri := redirectURIs[0]
		if idx := strings.Index(uri, "//"); idx != -1 {
			remainder := uri[idx+2:]
			if pathIdx := strings.Index(remainder, "/"); pathIdx != -1 {
				callbackPath = remainder[pathIdx:]
			}
		}
	}

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

	// Generate random client secret
	clientSecret := generateRandomString(32)

	// Create K8s secret in preview namespace with the OIDC client secret
	oidcSecretName := fmt.Sprintf("preview-oidc-client-secret-%s", prNumber)
	oidcSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      oidcSecretName,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"client_secret": clientSecret,
		},
	}
	if err := h.createOrUpdateTyped(ctx, oidcSecret); err != nil {
		return fmt.Errorf("failed to create OIDC client secret: %w", err)
	}

	// Update client ID and redirect URI
	newSpec["clientId"] = fmt.Sprintf("preview-%s-%s", appName, prNumber)
	newSpec["clientName"] = fmt.Sprintf("%s Preview PR #%s", appName, prNumber)
	newSpec["redirectUris"] = []interface{}{
		fmt.Sprintf("https://pr-%s-%s.%s%s", prNumber, appName, h.previewDomain, callbackPath),
	}
	// Point secretRef to the operator-created secret
	newSpec["secretRef"] = map[string]interface{}{
		"name": oidcSecretName,
		"key":  "client_secret",
	}

	if err := unstructured.SetNestedMap(previewOIDC.Object, newSpec, "spec"); err != nil {
		return fmt.Errorf("failed to set OIDCClient spec: %w", err)
	}

	// Create or update the OIDCClient
	if err := h.createOrUpdate(ctx, previewOIDC); err != nil {
		return err
	}

	h.log.Info("Created preview OIDCClient with managed secret", "name", previewOIDC.GetName(), "secret", oidcSecretName)
	return nil
}

// setupDatabase checks production namespace for postgres annotations and sets up CNPG
func (h *PreviewHandler) setupDatabase(ctx context.Context, namespace, appName, prNumber string) error {
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

	// CNPG cluster may live in a different namespace (e.g. shared "postgres" namespace)
	clusterNamespace := prodNS.Annotations["postgres.cnpg.io/cluster-namespace"]
	if clusterNamespace == "" {
		clusterNamespace = "postgres"
	}

	h.log.Info("App uses PostgreSQL, setting up CNPG cluster from snapshot", "cluster", clusterName, "clusterNamespace", clusterNamespace)

	// Snapshot the production primary and bootstrap a CNPG clone from it.
	// Names are PR-keyed (one preview per PR); see cloneCNPGFromSnapshot.
	c := cnpgClone{
		sourceCluster:    clusterName,
		sourceClusterNS:  clusterNamespace,
		targetNS:         namespace,
		prodSnapshotName: fmt.Sprintf("postgres-preview-pr-%s", prNumber),
		vscName:          fmt.Sprintf("postgres-preview-%s-content", prNumber),
		vsName:           "postgres-preview-snapshot",
		clusterName:      fmt.Sprintf("postgres-preview-%s", prNumber),
		snapshotClass:    "ceph-block-snapshot",
		labels: map[string]string{
			"preview.homelab/pr":        prNumber,
			"preview.homelab/namespace": namespace,
			"preview.homelab/type":      "database",
		},
	}
	if err := h.cloneCNPGFromSnapshot(ctx, c); err != nil {
		return err
	}
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

	if err := h.createOrUpdate(ctx, redis); err != nil {
		return err
	}

	h.log.Info("Created Redis instance")
	return nil
}

// setupS3Proxy creates an S3 proxy deployment for storage isolation
func (h *PreviewHandler) setupS3Proxy(ctx context.Context, namespace, bucket, prNumber string) error {
	// Create PVC for local storage
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "s3proxy-data",
			Namespace: namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: strPtr("rook-ceph-block"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("5Gi"),
				},
			},
		},
	}

	if err := h.createOrUpdateTyped(ctx, pvc); err != nil {
		return fmt.Errorf("failed to create PVC: %w", err)
	}

	// Create s3proxy credentials secret
	accessKey := fmt.Sprintf("preview-%s", prNumber)
	secretKey := fmt.Sprintf("preview-secret-%s", prNumber)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "s3proxy-credentials",
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"accessKeyId":     accessKey,
			"secretAccessKey": secretKey,
		},
	}

	if err := h.createOrUpdateTyped(ctx, secret); err != nil {
		return fmt.Errorf("failed to create s3proxy credentials: %w", err)
	}

	// Create s3proxy deployment
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "s3proxy",
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "s3proxy"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "s3proxy"},
					Annotations: map[string]string{
						"ambient.istio.io/redirection": "disabled",
					},
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup: int64Ptr(1000),
					},
					Containers: []corev1.Container{
						{
							Name:  "s3proxy",
							Image: "andrewgaul/s3proxy:sha-001d042",
							Ports: []corev1.ContainerPort{
								{ContainerPort: 8080, Name: "http"},
							},
							Env: []corev1.EnvVar{
								{
									Name: "S3PROXY_IDENTITY",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: "s3proxy-credentials"},
											Key:                  "accessKeyId",
										},
									},
								},
								{
									Name: "S3PROXY_CREDENTIAL",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{Name: "s3proxy-credentials"},
											Key:                  "secretAccessKey",
										},
									},
								},
								{Name: "S3PROXY_ENDPOINT", Value: "http://0.0.0.0:8080"},
								{Name: "JCLOUDS_PROVIDER", Value: "filesystem"},
								{Name: "JCLOUDS_FILESYSTEM_BASEDIR", Value: "/data/s3"},
							},
							SecurityContext: &corev1.SecurityContext{
								RunAsUser:  int64Ptr(1000),
								RunAsGroup: int64Ptr(1000),
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "data", MountPath: "/data/s3"},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/bin/sh", "-c", `wget -q --timeout=5 -O /dev/null http://localhost:8080/ 2>&1 || [ "$?" = "8" ]`},
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
								TimeoutSeconds:      10,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/bin/sh", "-c", `wget -q --timeout=5 -O /dev/null http://localhost:8080/ 2>&1 || [ "$?" = "8" ]`},
									},
								},
								InitialDelaySeconds: 60,
								PeriodSeconds:       15,
								TimeoutSeconds:      10,
								FailureThreshold:    5,
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "s3proxy-data",
								},
							},
						},
					},
				},
			},
		},
	}

	if err := h.createOrUpdateTyped(ctx, deployment); err != nil {
		return fmt.Errorf("failed to create s3proxy deployment: %w", err)
	}

	// Create s3proxy service
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "s3proxy",
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "s3proxy"},
			Ports: []corev1.ServicePort{
				{Port: 8080, TargetPort: intstr.FromInt(8080), Name: "http"},
			},
		},
	}

	if err := h.createOrUpdateTyped(ctx, service); err != nil {
		return fmt.Errorf("failed to create s3proxy service: %w", err)
	}

	h.log.Info("Created S3 proxy", "bucket", bucket)
	return nil
}

// patchWorkload patches the Deployment or HelmRelease with preview values
func (h *PreviewHandler) patchWorkload(ctx context.Context, namespace, appName, prNumber string, config *v1.PreviewConfig) error {
	switch config.Spec.DeploymentType {
	case v1.DeploymentTypeHelm:
		return h.patchHelmRelease(ctx, namespace, appName, prNumber, config)
	default:
		return h.patchDeployment(ctx, namespace, appName, prNumber, config)
	}
}

// patchDeployment patches a Deployment with preview env vars
func (h *PreviewHandler) patchDeployment(ctx context.Context, namespace, appName, prNumber string, config *v1.PreviewConfig) error {
	if config.Spec.EnvMapping == nil {
		h.log.Info("No envMapping defined, skipping deployment patching")
		return nil
	}

	// Get the deployment
	var deployment appsv1.Deployment
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: appName}, &deployment); err != nil {
		if errors.IsNotFound(err) {
			h.log.Info("Deployment not found, will retry later", "name", appName)
			return nil // Deployment might not exist yet
		}
		return err
	}

	// Build env var patches
	envPatches := h.buildEnvPatches(namespace, appName, prNumber, config)

	// Apply patches to all containers
	modified := false
	for i := range deployment.Spec.Template.Spec.Containers {
		container := &deployment.Spec.Template.Spec.Containers[i]
		for _, patch := range envPatches {
			if h.setOrUpdateEnv(container, patch.Name, patch.Value, patch.ValueFrom) {
				modified = true
			}
		}
	}

	if modified {
		if err := h.client.Update(ctx, &deployment); err != nil {
			return fmt.Errorf("failed to update deployment: %w", err)
		}
		h.log.Info("Patched deployment with preview env vars", "name", appName)
	}

	return nil
}

// patchHelmRelease patches a HelmRelease with preview values
func (h *PreviewHandler) patchHelmRelease(ctx context.Context, namespace, appName, prNumber string, config *v1.PreviewConfig) error {
	if config.Spec.HelmValues == nil {
		h.log.Info("No helmValues defined, skipping HelmRelease patching")
		return nil
	}

	// Get the HelmRelease
	hr := &unstructured.Unstructured{}
	hr.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "helm.toolkit.fluxcd.io",
		Version: "v2",
		Kind:    "HelmRelease",
	})

	if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: appName}, hr); err != nil {
		if errors.IsNotFound(err) {
			h.log.Info("HelmRelease not found, will retry later", "name", appName)
			return nil
		}
		return err
	}

	// Get or create spec.values
	values, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	if values == nil {
		values = make(map[string]interface{})
	}

	// Build value patches (nil dbCreds — this legacy path doesn't resolve secrets)
	patches := h.buildHelmValuePatches(namespace, appName, prNumber, config, nil)

	// Apply patches
	modified := false
	for path, value := range patches {
		if h.setNestedValue(values, path, value) {
			modified = true
		}
	}

	if modified {
		if err := unstructured.SetNestedMap(hr.Object, values, "spec", "values"); err != nil {
			return err
		}
		if err := h.client.Update(ctx, hr); err != nil {
			return fmt.Errorf("failed to update HelmRelease: %w", err)
		}
		h.log.Info("Patched HelmRelease with preview values", "name", appName)
	}

	return nil
}

// EnvPatch represents an environment variable patch
type EnvPatch struct {
	Name      string
	Value     string
	ValueFrom *corev1.EnvVarSource
}

// buildEnvPatches builds the list of env var patches based on config
func (h *PreviewHandler) buildEnvPatches(namespace, appName, prNumber string, config *v1.PreviewConfig) []EnvPatch {
	mapping := config.Spec.EnvMapping
	var patches []EnvPatch

	// App URL
	if mapping.AppURL != "" {
		patches = append(patches, EnvPatch{
			Name:  mapping.AppURL,
			Value: fmt.Sprintf("https://pr-%s-%s.%s", prNumber, appName, h.previewDomain),
		})
	}

	// OIDC
	if mapping.OIDCClientID != "" {
		patches = append(patches, EnvPatch{
			Name:  mapping.OIDCClientID,
			Value: fmt.Sprintf("preview-%s-%s", appName, prNumber),
		})
	}
	if mapping.OIDCClientSecret != "" {
		patches = append(patches, EnvPatch{
			Name: mapping.OIDCClientSecret,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "preview-oidc-client-secret"},
					Key:                  "client_secret",
				},
			},
		})
	}

	// Database
	if mapping.DatabaseHost != "" {
		patches = append(patches, EnvPatch{
			Name:  mapping.DatabaseHost,
			Value: fmt.Sprintf("postgres-preview-%s-rw.%s.svc.cluster.local", prNumber, namespace),
		})
	}
	if mapping.DatabaseName != "" {
		patches = append(patches, EnvPatch{
			Name:  mapping.DatabaseName,
			Value: appName, // Original app database name
		})
	}
	if mapping.DatabaseUser != "" {
		patches = append(patches, EnvPatch{
			Name: mapping.DatabaseUser,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: fmt.Sprintf("postgres-preview-%s-superuser", prNumber)},
					Key:                  "username",
				},
			},
		})
	}
	if mapping.DatabasePassword != "" {
		patches = append(patches, EnvPatch{
			Name: mapping.DatabasePassword,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: fmt.Sprintf("postgres-preview-%s-superuser", prNumber)},
					Key:                  "password",
				},
			},
		})
	}

	// Redis
	if mapping.RedisHost != "" {
		patches = append(patches, EnvPatch{
			Name:  mapping.RedisHost,
			Value: fmt.Sprintf("redis-preview.%s.svc.cluster.local", namespace),
		})
	}
	if mapping.RedisPort != "" {
		patches = append(patches, EnvPatch{
			Name:  mapping.RedisPort,
			Value: "6379",
		})
	}

	// S3
	if mapping.S3Endpoint != "" {
		patches = append(patches, EnvPatch{
			Name:  mapping.S3Endpoint,
			Value: fmt.Sprintf("http://s3proxy.%s.svc.cluster.local:8080", namespace),
		})
	}
	if mapping.S3AccessKey != "" {
		patches = append(patches, EnvPatch{
			Name: mapping.S3AccessKey,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "s3proxy-credentials"},
					Key:                  "accessKeyId",
				},
			},
		})
	}
	if mapping.S3SecretKey != "" {
		patches = append(patches, EnvPatch{
			Name: mapping.S3SecretKey,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "s3proxy-credentials"},
					Key:                  "secretAccessKey",
				},
			},
		})
	}
	if mapping.S3Bucket != "" {
		patches = append(patches, EnvPatch{
			Name:  mapping.S3Bucket,
			Value: appName,
		})
	}
	if mapping.S3Region != "" {
		patches = append(patches, EnvPatch{
			Name:  mapping.S3Region,
			Value: "preview",
		})
	}

	// Arbitrary extra env overrides (repoint stripped-secret env at the preview
	// S3 proxy, or set dummy values). These override any existing valueFrom; the
	// patch renderer clears valueFrom for plain-value entries.
	if len(mapping.ExtraEnv) != 0 {
		s3Endpoint := fmt.Sprintf("http://s3proxy.%s.svc.cluster.local:8080", namespace)
		s3AccessKey := fmt.Sprintf("preview-%s", prNumber)
		s3SecretKey := fmt.Sprintf("preview-secret-%s", prNumber)
		repl := strings.NewReplacer(
			"{S3_ENDPOINT}", s3Endpoint,
			"{S3_ACCESS_KEY}", s3AccessKey,
			"{S3_SECRET_KEY}", s3SecretKey,
			"{NAMESPACE}", namespace,
			"{PR_NUMBER}", prNumber,
			"{APP_NAME}", appName,
		)
		for k, v := range mapping.ExtraEnv {
			patches = append(patches, EnvPatch{Name: k, Value: repl.Replace(v)})
		}
	}

	return patches
}

// buildHelmValuePatches builds the map of Helm value patches based on config
func (h *PreviewHandler) buildHelmValuePatches(namespace, appName, prNumber string, config *v1.PreviewConfig, dbCreds *DBCredentials) map[string]string {
	mapping := config.Spec.HelmValues
	patches := make(map[string]string)

	// App URL
	if mapping.AppURL != "" {
		patches[mapping.AppURL] = fmt.Sprintf("https://pr-%s-%s.%s", prNumber, appName, h.previewDomain)
	}

	// OIDC
	if mapping.OIDCClientID != "" {
		patches[mapping.OIDCClientID] = fmt.Sprintf("preview-%s-%s", appName, prNumber)
	}

	// Database
	if mapping.DatabaseHost != "" {
		patches[mapping.DatabaseHost] = fmt.Sprintf("postgres-preview-%s-rw.%s.svc.cluster.local", prNumber, namespace)
	}
	if mapping.DatabaseName != "" {
		patches[mapping.DatabaseName] = appName
	}
	if dbCreds != nil {
		if mapping.DatabaseUser != "" {
			patches[mapping.DatabaseUser] = dbCreds.Username
		}
		if mapping.DatabasePassword != "" {
			patches[mapping.DatabasePassword] = dbCreds.Password
		}
	}

	// Redis
	if mapping.RedisHost != "" {
		patches[mapping.RedisHost] = fmt.Sprintf("redis-preview.%s.svc.cluster.local", namespace)
	}
	if mapping.RedisPort != "" {
		patches[mapping.RedisPort] = "6379"
	}

	// S3
	if mapping.S3Endpoint != "" {
		patches[mapping.S3Endpoint] = fmt.Sprintf("http://s3proxy.%s.svc.cluster.local:8080", namespace)
	}
	if mapping.S3Bucket != "" {
		patches[mapping.S3Bucket] = appName
	}

	// Extra values (arbitrary overrides with variable substitution)
	redisHost := fmt.Sprintf("redis-preview.%s.svc.cluster.local", namespace)
	dbHost := fmt.Sprintf("postgres-preview-%s-rw.%s.svc.cluster.local", prNumber, namespace)
	for k, v := range mapping.ExtraValues {
		v = strings.ReplaceAll(v, "{REDIS_HOST}", redisHost)
		v = strings.ReplaceAll(v, "{DB_HOST}", dbHost)
		v = strings.ReplaceAll(v, "{NAMESPACE}", namespace)
		v = strings.ReplaceAll(v, "{PR_NUMBER}", prNumber)
		v = strings.ReplaceAll(v, "{APP_NAME}", appName)
		patches[k] = v
	}

	return patches
}

// ensureHelmReleaseValues directly patches the HelmRelease's spec.values via the
// Kubernetes API. This works around kustomize's inability to deep-merge spec.values
// on HelmRelease (CRD uses x-kubernetes-preserve-unknown-fields: true, causing
// strategic merge to not overwrite existing keys).
// Returns true if the HelmRelease was patched, false if values were already correct.
func (h *PreviewHandler) ensureHelmReleaseValues(ctx context.Context, namespace, appName string) (bool, error) {
	prNumber := strings.TrimPrefix(namespace, "preview-pr-")

	config, err := h.fetchPreviewConfig(ctx, namespace, appName)
	if err != nil {
		return false, nil
	}

	if config.Spec.DeploymentType != v1.DeploymentTypeHelm || config.Spec.HelmValues == nil {
		return false, nil
	}

	// Read DB credentials if available
	var dbCreds *DBCredentials
	secretName := fmt.Sprintf("postgres-preview-%s-superuser", prNumber)
	secret := &corev1.Secret{}
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, secret); err == nil {
		dbCreds = &DBCredentials{
			Username: string(secret.Data["username"]),
			Password: string(secret.Data["password"]),
		}
	}

	// Read the HelmRelease
	hr := &unstructured.Unstructured{}
	hr.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "helm.toolkit.fluxcd.io",
		Version: "v2",
		Kind:    "HelmRelease",
	})
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: appName}, hr); err != nil {
		return false, nil // Not created yet
	}

	// Get current spec.values
	values, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	if values == nil {
		values = make(map[string]interface{})
	}

	// Build the full value patches
	patches := h.buildHelmValuePatches(namespace, appName, prNumber, config, dbCreds)

	// Check if values already match
	modified := false
	for path, value := range patches {
		if h.setNestedValue(values, path, value) {
			modified = true
		}
	}

	// Also strip valuesFrom if still present
	valuesFrom, found, _ := unstructured.NestedSlice(hr.Object, "spec", "valuesFrom")
	if found && len(valuesFrom) > 0 {
		if err := unstructured.SetNestedSlice(hr.Object, []interface{}{}, "spec", "valuesFrom"); err != nil {
			return false, err
		}
		modified = true
	}

	if !modified {
		return false, nil
	}

	if err := unstructured.SetNestedMap(hr.Object, values, "spec", "values"); err != nil {
		return false, err
	}
	if err := h.client.Update(ctx, hr); err != nil {
		return false, fmt.Errorf("failed to directly patch HelmRelease values: %w", err)
	}

	h.log.Info("Directly patched HelmRelease with preview values", "app", appName)
	return true, nil
}

// setOrUpdateEnv sets or updates an environment variable in a container
func (h *PreviewHandler) setOrUpdateEnv(container *corev1.Container, name, value string, valueFrom *corev1.EnvVarSource) bool {
	for i, env := range container.Env {
		if env.Name == name {
			if valueFrom != nil {
				if container.Env[i].ValueFrom == nil || *container.Env[i].ValueFrom != *valueFrom {
					container.Env[i].Value = ""
					container.Env[i].ValueFrom = valueFrom
					return true
				}
			} else if container.Env[i].Value != value {
				container.Env[i].Value = value
				container.Env[i].ValueFrom = nil
				return true
			}
			return false
		}
	}
	// Add new env var
	newEnv := corev1.EnvVar{Name: name}
	if valueFrom != nil {
		newEnv.ValueFrom = valueFrom
	} else {
		newEnv.Value = value
	}
	container.Env = append(container.Env, newEnv)
	return true
}

// setNestedValue sets a nested value in a map using dot notation path
func (h *PreviewHandler) setNestedValue(m map[string]interface{}, path string, value string) bool {
	parts := strings.Split(path, ".")
	current := m

	for i, part := range parts[:len(parts)-1] {
		if next, ok := current[part].(map[string]interface{}); ok {
			current = next
		} else {
			// Create intermediate maps
			newMap := make(map[string]interface{})
			current[part] = newMap
			current = newMap
			_ = i // Avoid unused variable warning
		}
	}

	lastPart := parts[len(parts)-1]
	if current[lastPart] != value {
		current[lastPart] = value
		return true
	}
	return false
}

// createOrUpdate creates or updates an unstructured resource
func (h *PreviewHandler) createOrUpdate(ctx context.Context, obj *unstructured.Unstructured) error {
	if err := h.client.Create(ctx, obj); err != nil {
		if errors.IsAlreadyExists(err) {
			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(obj.GroupVersionKind())
			if err := h.client.Get(ctx, types.NamespacedName{
				Namespace: obj.GetNamespace(),
				Name:      obj.GetName(),
			}, existing); err != nil {
				return err
			}
			obj.SetResourceVersion(existing.GetResourceVersion())
			return h.client.Update(ctx, obj)
		}
		return err
	}
	return nil
}

// createOrUpdateTyped creates or updates a typed resource
func (h *PreviewHandler) createOrUpdateTyped(ctx context.Context, obj client.Object) error {
	if err := h.client.Create(ctx, obj); err != nil {
		if errors.IsAlreadyExists(err) {
			existing := obj.DeepCopyObject().(client.Object)
			if err := h.client.Get(ctx, types.NamespacedName{
				Namespace: obj.GetNamespace(),
				Name:      obj.GetName(),
			}, existing); err != nil {
				return err
			}
			obj.SetResourceVersion(existing.GetResourceVersion())
			return h.client.Update(ctx, obj)
		}
		return err
	}
	return nil
}

// Helper functions
func strPtr(s string) *string {
	return &s
}

func int64Ptr(i int64) *int64 {
	return &i
}

// hashPreviewConfigSpec returns a stable hash of the PreviewConfig spec, used to
// detect config edits so an in-flight preview can be re-rendered.
func hashPreviewConfigSpec(config *v1.PreviewConfig) string {
	if config == nil {
		return ""
	}
	b, err := json.Marshal(config.Spec)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// PreviewConfigHash fetches the app's PreviewConfig and returns its spec hash.
// A missing PreviewConfig hashes as the empty spec (consistent with prepare).
func (h *PreviewHandler) PreviewConfigHash(ctx context.Context, namespace, appName string) (string, error) {
	config, err := h.fetchPreviewConfig(ctx, namespace, appName)
	if err != nil {
		if errors.IsNotFound(err) {
			config = &v1.PreviewConfig{}
		} else {
			return "", err
		}
	}
	return hashPreviewConfigSpec(config), nil
}

// TeardownSnapshots deletes the cluster-scoped VolumeSnapshotContents (created
// with deletionPolicy: Retain for the cross-namespace restore) and the prod-
// namespace VolumeSnapshots that the config-volume and CNPG clone paths create.
// Namespace deletion can't GC these, so without this they leak orphaned Ceph
// snapshots. Deletes are best-effort and idempotent (NotFound is ignored); the
// prod VolumeSnapshot's own (Delete-policy) content reaps the underlying snapshot.
func (h *PreviewHandler) TeardownSnapshots(ctx context.Context, appName, prNumber string) error {
	deleteByGVK := func(kind, namespace, name string) error {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "snapshot.storage.k8s.io",
			Version: "v1",
			Kind:    kind,
		})
		if namespace != "" {
			obj.SetNamespace(namespace)
		}
		obj.SetName(name)
		if err := h.client.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete %s %s/%s: %w", kind, namespace, name, err)
		}
		return nil
	}

	// Cluster-scoped VolumeSnapshotContents (Retain) — config-volume + CNPG.
	for _, name := range []string{
		fmt.Sprintf("config-preview-%s-pr-%s-content", appName, prNumber),
		fmt.Sprintf("postgres-preview-%s-content", prNumber),
	} {
		if err := deleteByGVK("VolumeSnapshotContent", "", name); err != nil {
			return err
		}
	}

	// Prod-namespace VolumeSnapshots. The config-volume snapshot lives in the app
	// namespace; the CNPG snapshot lives in the cluster namespace (annotation,
	// default "postgres").
	if err := deleteByGVK("VolumeSnapshot", appName, fmt.Sprintf("config-preview-pr-%s", prNumber)); err != nil {
		return err
	}
	var prodNS corev1.Namespace
	if err := h.client.Get(ctx, types.NamespacedName{Name: appName}, &prodNS); err == nil {
		if prodNS.Annotations["postgres.cnpg.io/cluster-name"] != "" {
			cns := prodNS.Annotations["postgres.cnpg.io/cluster-namespace"]
			if cns == "" {
				cns = "postgres"
			}
			if err := deleteByGVK("VolumeSnapshot", cns, fmt.Sprintf("postgres-preview-pr-%s", prNumber)); err != nil {
				return err
			}
		}
	}
	return nil
}
