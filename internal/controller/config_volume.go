package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

// setupConfigVolume snapshots a production config PVC, clones it into the preview
// namespace, runs a URL-rewrite Job, and patches the Deployment to use the clone.
func (h *PreviewHandler) setupConfigVolume(ctx context.Context, namespace, appName, prNumber string, config *v1.PreviewConfig) error {
	cv := config.Spec.ConfigVolume
	snapshotClass := cv.SnapshotClass
	if snapshotClass == "" {
		snapshotClass = "ceph-block-snapshot"
	}

	clonePVCName := fmt.Sprintf("%s-config-clone", appName)
	prodSnapshotName := fmt.Sprintf("config-preview-pr-%s", prNumber)
	vscName := fmt.Sprintf("config-preview-%s-pr-%s-content", appName, prNumber)
	previewSnapshotName := fmt.Sprintf("%s-config-snapshot", appName)
	rewriteJobName := fmt.Sprintf("config-rewrite-%s", appName)

	// Step A: Create on-demand VolumeSnapshot in production namespace
	h.log.Info("Creating config volume snapshot in production namespace", "namespace", appName, "pvc", cv.ClaimName)

	prodSnapshot := &unstructured.Unstructured{}
	prodSnapshot.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "snapshot.storage.k8s.io",
		Version: "v1",
		Kind:    "VolumeSnapshot",
	})
	prodSnapshot.SetNamespace(appName)
	prodSnapshot.SetName(prodSnapshotName)
	prodSnapshot.SetLabels(map[string]string{
		"preview.homelab/pr":   prNumber,
		"preview.homelab/type": "config",
	})

	vsSpec := map[string]interface{}{
		"volumeSnapshotClassName": snapshotClass,
		"source": map[string]interface{}{
			"persistentVolumeClaimName": cv.ClaimName,
		},
	}
	if err := unstructured.SetNestedMap(prodSnapshot.Object, vsSpec, "spec"); err != nil {
		return fmt.Errorf("failed to set production snapshot spec: %w", err)
	}

	if err := h.createOrUpdate(ctx, prodSnapshot); err != nil {
		return fmt.Errorf("failed to create production VolumeSnapshot: %w", err)
	}

	// Wait for snapshot to become ready
	if err := h.waitForSnapshotReady(ctx, appName, prodSnapshotName); err != nil {
		return fmt.Errorf("production snapshot not ready: %w", err)
	}

	// Step B: Read snapshot metadata and create cross-namespace clone
	// Re-read to get status fields
	prodSnapshot = &unstructured.Unstructured{}
	prodSnapshot.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "snapshot.storage.k8s.io",
		Version: "v1",
		Kind:    "VolumeSnapshot",
	})
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: appName, Name: prodSnapshotName}, prodSnapshot); err != nil {
		return fmt.Errorf("failed to re-read production snapshot: %w", err)
	}

	boundVSCName, _, _ := unstructured.NestedString(prodSnapshot.Object, "status", "boundVolumeSnapshotContentName")
	if boundVSCName == "" {
		return fmt.Errorf("production snapshot has no boundVolumeSnapshotContentName")
	}

	// Read the bound VolumeSnapshotContent for snapshotHandle, driver, restoreSize
	boundVSC := &unstructured.Unstructured{}
	boundVSC.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "snapshot.storage.k8s.io",
		Version: "v1",
		Kind:    "VolumeSnapshotContent",
	})
	if err := h.client.Get(ctx, types.NamespacedName{Name: boundVSCName}, boundVSC); err != nil {
		return fmt.Errorf("failed to get bound VolumeSnapshotContent: %w", err)
	}

	snapshotHandle, _, _ := unstructured.NestedString(boundVSC.Object, "status", "snapshotHandle")
	if snapshotHandle == "" {
		return fmt.Errorf("bound VolumeSnapshotContent has no snapshotHandle")
	}
	driver, _, _ := unstructured.NestedString(boundVSC.Object, "spec", "driver")

	restoreSize, _, _ := unstructured.NestedString(prodSnapshot.Object, "status", "restoreSize")
	if restoreSize == "" {
		restoreSize = "5Gi"
	}

	h.log.Info("Got snapshot metadata", "handle", snapshotHandle, "driver", driver, "restoreSize", restoreSize)

	// Create new VolumeSnapshotContent (cluster-scoped) pointing to preview namespace
	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "snapshot.storage.k8s.io",
		Version: "v1",
		Kind:    "VolumeSnapshotContent",
	})
	vsc.SetName(vscName)
	vsc.SetLabels(map[string]string{
		"preview.homelab/pr":        prNumber,
		"preview.homelab/namespace": namespace,
		"preview.homelab/type":      "config",
	})

	vscSpec := map[string]interface{}{
		"driver":                  driver,
		"deletionPolicy":          "Retain",
		"source":                  map[string]interface{}{"snapshotHandle": snapshotHandle},
		"volumeSnapshotClassName": snapshotClass,
		"volumeSnapshotRef": map[string]interface{}{
			"name":      previewSnapshotName,
			"namespace": namespace,
		},
	}
	if err := unstructured.SetNestedMap(vsc.Object, vscSpec, "spec"); err != nil {
		return err
	}

	if err := h.createOrUpdate(ctx, vsc); err != nil {
		return fmt.Errorf("failed to create VolumeSnapshotContent: %w", err)
	}

	// Create VolumeSnapshot in preview namespace
	previewSnapshot := &unstructured.Unstructured{}
	previewSnapshot.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "snapshot.storage.k8s.io",
		Version: "v1",
		Kind:    "VolumeSnapshot",
	})
	previewSnapshot.SetNamespace(namespace)
	previewSnapshot.SetName(previewSnapshotName)

	previewVSSpec := map[string]interface{}{
		"source": map[string]interface{}{
			"volumeSnapshotContentName": vscName,
		},
	}
	if err := unstructured.SetNestedMap(previewSnapshot.Object, previewVSSpec, "spec"); err != nil {
		return err
	}

	if err := h.createOrUpdate(ctx, previewSnapshot); err != nil {
		return fmt.Errorf("failed to create preview VolumeSnapshot: %w", err)
	}

	// Step C: Create PVC from snapshot in preview namespace
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clonePVCName,
			Namespace: namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: strPtr("rook-ceph-block"),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(restoreSize),
				},
			},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: strPtr("snapshot.storage.k8s.io"),
				Kind:     "VolumeSnapshot",
				Name:     previewSnapshotName,
			},
		},
	}

	if err := h.createOrUpdateTyped(ctx, pvc); err != nil {
		return fmt.Errorf("failed to create cloned PVC: %w", err)
	}

	// Step D: Run URL rewrite Job
	if err := h.runURLRewriteJob(ctx, namespace, appName, prNumber, clonePVCName, rewriteJobName); err != nil {
		return fmt.Errorf("failed to run URL rewrite job: %w", err)
	}

	// Step E: Patch Deployment volume to use cloned PVC
	if err := h.patchDeploymentVolume(ctx, namespace, appName, cv.ClaimName, clonePVCName); err != nil {
		return fmt.Errorf("failed to patch deployment volume: %w", err)
	}

	h.log.Info("Config volume clone and rewrite complete", "app", appName, "clonePVC", clonePVCName)
	return nil
}

// runURLRewriteJob creates and waits for a Job that rewrites service URLs in config files.
func (h *PreviewHandler) runURLRewriteJob(ctx context.Context, namespace, appName, prNumber, clonePVCName, jobName string) error {
	// Check if job already succeeded
	var existingJob batchv1.Job
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: jobName}, &existingJob); err == nil {
		if existingJob.Status.Succeeded > 0 {
			h.log.Info("URL rewrite job already succeeded, skipping", "job", jobName)
			return nil
		}
	}

	ttlSeconds := int32(300)
	backoffLimit := int32(2)
	runAsUser := int64(0) // Need root to write to PVC owned by app user
	rewriteScript := fmt.Sprintf(`set -e
echo "Rewriting URLs for preview namespace..."
PROD_NS="%s"
PREVIEW_NS="%s"
DOMAIN="%s"
PR="%s"
APP="%s"

# Install file command if not present
apk add --no-cache file >/dev/null 2>&1 || true

find /config -type f | while read -r file; do
  if file "$file" | grep -qiE 'text|xml|json|ascii|utf-'; then
    # Rewrite internal FQDNs: .{prodNs}.svc.cluster.local -> .{previewNs}.svc.cluster.local
    sed -i "s/\.${PROD_NS}\.svc\.cluster\.local/.${PREVIEW_NS}.svc.cluster.local/g" "$file"
    # Rewrite external URLs: {app}.{domain} -> pr-{pr}-{app}.{domain}
    sed -i "s/${APP}\.${DOMAIN}/pr-${PR}-${APP}.${DOMAIN}/g" "$file"
  fi
done
echo "URL rewrite complete"
`, appName, namespace, h.previewDomain, prNumber, appName)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"preview.homelab/pr":   prNumber,
				"preview.homelab/type": "config-rewrite",
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttlSeconds,
			BackoffLimit:            &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"ambient.istio.io/redirection": "disabled",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser: &runAsUser,
					},
					Containers: []corev1.Container{
						{
							Name:    "rewrite",
							Image:   "alpine:3.21",
							Command: []string{"/bin/sh", "-c", rewriteScript},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "config",
									MountPath: "/config",
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: clonePVCName,
								},
							},
						},
					},
				},
			},
		},
	}

	// Create the job (don't update if it already exists and hasn't succeeded)
	if err := h.client.Create(ctx, job); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create rewrite job: %w", err)
		}
		// Already exists but not succeeded - just wait for it
	}

	// Wait for job completion
	if err := h.waitForJobCompletion(ctx, namespace, jobName); err != nil {
		return fmt.Errorf("URL rewrite job failed: %w", err)
	}

	return nil
}

// patchDeploymentVolume swaps the PVC claim in the Deployment volumes.
func (h *PreviewHandler) patchDeploymentVolume(ctx context.Context, namespace, appName, originalClaimName, clonePVCName string) error {
	var deployment appsv1.Deployment
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: appName}, &deployment); err != nil {
		if errors.IsNotFound(err) {
			h.log.Info("Deployment not found yet for volume patch, will retry later", "name", appName)
			return nil
		}
		return err
	}

	modified := false
	for i := range deployment.Spec.Template.Spec.Volumes {
		vol := &deployment.Spec.Template.Spec.Volumes[i]
		if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == originalClaimName {
			if vol.PersistentVolumeClaim.ClaimName != clonePVCName {
				vol.PersistentVolumeClaim.ClaimName = clonePVCName
				modified = true
			}
		}
	}

	if modified {
		if err := h.client.Update(ctx, &deployment); err != nil {
			return fmt.Errorf("failed to update deployment volume: %w", err)
		}
		h.log.Info("Patched deployment volume to use cloned PVC", "deployment", appName, "clonePVC", clonePVCName)
	}

	return nil
}

// waitForSnapshotReady polls until a VolumeSnapshot's readyToUse is true.
func (h *PreviewHandler) waitForSnapshotReady(ctx context.Context, namespace, name string) error {
	timeout := time.After(120 * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timed out waiting for VolumeSnapshot %s/%s to become ready", namespace, name)
		case <-ticker.C:
			snapshot := &unstructured.Unstructured{}
			snapshot.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "snapshot.storage.k8s.io",
				Version: "v1",
				Kind:    "VolumeSnapshot",
			})
			if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, snapshot); err != nil {
				return err
			}

			ready, found, _ := unstructured.NestedBool(snapshot.Object, "status", "readyToUse")
			if found && ready {
				h.log.Info("VolumeSnapshot is ready", "namespace", namespace, "name", name)
				return nil
			}
		}
	}
}

// waitForJobCompletion polls until a Job succeeds or fails.
func (h *PreviewHandler) waitForJobCompletion(ctx context.Context, namespace, name string) error {
	timeout := time.After(120 * time.Second)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timed out waiting for Job %s/%s to complete", namespace, name)
		case <-ticker.C:
			var job batchv1.Job
			if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &job); err != nil {
				return err
			}

			if job.Status.Succeeded > 0 {
				h.log.Info("URL rewrite job completed successfully", "job", name)
				return nil
			}

			if job.Status.Failed > 0 {
				return fmt.Errorf("URL rewrite job %s failed", name)
			}
		}
	}
}
