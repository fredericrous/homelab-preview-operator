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

// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// PersistentVolumes are read-only and cluster-wide. The manager client reads through an
// informer cache, which needs list and watch beside get.

// setupConfigVolume snapshots a production config PVC, clones it into the preview
// namespace, runs a URL-rewrite Job, and patches the Deployment to use the clone.
func (h *PreviewHandler) setupConfigVolume(ctx context.Context, namespace, appName, prNumber string, config *v1.PreviewConfig) error {
	cv := config.Spec.ConfigVolume
	if cv.Mode == v1.ConfigVolumeModeNFS {
		// The NFS swap is a Kustomization patch built by buildAllPatches; nothing to create here.
		return nil
	}
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

	// Create-only: a retry finds the clone already bound, and a bound claim's spec is immutable.
	if err := h.createIfAbsent(ctx, pvc); err != nil {
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
	rewriteScript := snapshotRewriteScript(appName, namespace, h.previewDomain, prNumber)

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

// snapshotRewriteScript is the URL-rewrite script of the snapshot-mode Job; a golden test pins it.
func snapshotRewriteScript(appName, namespace, previewDomain, prNumber string) string {
	return fmt.Sprintf(`set -e
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
`, appName, namespace, previewDomain, prNumber, appName)
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

const (
	defaultNFSSizeLimit = "256Mi"
	nfsSeedContainer    = "config-seed"
	nfsSeedSourceVolume = "config-seed-src"
)

// nfsConfigVolume is the resolved input of the mode: nfs Deployment patch.
type nfsConfigVolume struct {
	// VolumeName is the production Deployment's volume that claimed the config PVC.
	VolumeName string
	SizeLimit  resource.Quantity
	// Copy is true when the temp dir is seeded from the export.
	Copy bool
	// Server and Path are the PV's NFS export; set only when Copy is true.
	Server string
	Path   string
	// PVName is the bound PV; set only when Copy is true.
	PVName string
	// RunAsUser and RunAsGroup own the seeded files; nil leaves them world-writable.
	RunAsUser  *int64
	RunAsGroup *int64
}

// resolveNFSConfigVolume reads the cluster state mode: nfs needs before patches are built.
// It returns nil when the config does not use mode: nfs.
func (h *PreviewHandler) resolveNFSConfigVolume(ctx context.Context, appName string, config *v1.PreviewConfig) (*nfsConfigVolume, error) {
	cv := config.Spec.ConfigVolume
	if cv == nil || cv.Mode != v1.ConfigVolumeModeNFS {
		return nil, nil
	}
	if config.Spec.DeploymentType == v1.DeploymentTypeHelm {
		return nil, fmt.Errorf("mode: nfs supports raw-manifest Deployments only; Helm-rendered Deployments are not patched")
	}

	sizeLimit := resource.MustParse(defaultNFSSizeLimit)
	copySeed := false
	if cv.NFS != nil {
		if cv.NFS.SizeLimit != nil {
			sizeLimit = *cv.NFS.SizeLimit
		}
		if cv.NFS.Copy != nil {
			copySeed = *cv.NFS.Copy
		}
	}

	var deployment appsv1.Deployment
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: appName, Name: appName}, &deployment); err != nil {
		return nil, fmt.Errorf("configVolume.claimName %q: cannot read production Deployment %q in namespace %q: %w", cv.ClaimName, appName, appName, err)
	}
	volumeName := ""
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == cv.ClaimName {
			volumeName = volume.Name
			break
		}
	}
	if volumeName == "" {
		return nil, fmt.Errorf("configVolume.claimName %q is not used as a persistentVolumeClaim volume by Deployment %q in namespace %q", cv.ClaimName, appName, appName)
	}

	resolved := &nfsConfigVolume{VolumeName: volumeName, SizeLimit: sizeLimit, Copy: copySeed}
	if !copySeed {
		h.log.Info("NFS config volume patched", "app", appName, "copy", false, "pv", "")
		return resolved, nil
	}

	var claim corev1.PersistentVolumeClaim
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: appName, Name: cv.ClaimName}, &claim); err != nil {
		return nil, fmt.Errorf("configVolume.claimName %q in namespace %q: cannot read PVC: %w", cv.ClaimName, appName, err)
	}
	if claim.Spec.VolumeName == "" {
		return nil, fmt.Errorf("configVolume.claimName %q in namespace %q is not bound to a PV; nfs.copy: true needs a bound NFS PV. Set nfs.copy: false to start empty.", cv.ClaimName, appName)
	}
	var volume corev1.PersistentVolume
	if err := h.client.Get(ctx, types.NamespacedName{Name: claim.Spec.VolumeName}, &volume); err != nil {
		return nil, fmt.Errorf("configVolume.claimName %q: cannot read PV %q: %w", cv.ClaimName, claim.Spec.VolumeName, err)
	}
	if volume.Spec.NFS == nil {
		return nil, fmt.Errorf("configVolume.claimName %q is bound to PV %q, which has no spec.nfs; nfs.copy: true needs an NFS PV. Set nfs.copy: false to start empty, or use mode: snapshot.", cv.ClaimName, volume.Name)
	}
	resolved.Server = volume.Spec.NFS.Server
	resolved.Path = volume.Spec.NFS.Path
	resolved.PVName = volume.Name
	resolved.RunAsUser, resolved.RunAsGroup = appRunAsIDs(&deployment, appName)

	h.log.Info("NFS config volume patched", "app", appName, "copy", true, "pv", volume.Name)
	return resolved, nil
}

// appRunAsIDs returns the uid and gid the app runs as: the app container's securityContext,
// else the pod's. The app container is the one named after the app, else the first.
func appRunAsIDs(deployment *appsv1.Deployment, appName string) (user, group *int64) {
	podSpec := deployment.Spec.Template.Spec
	if podSpec.SecurityContext != nil {
		user, group = podSpec.SecurityContext.RunAsUser, podSpec.SecurityContext.RunAsGroup
	}
	var app *corev1.Container
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == appName {
			app = &podSpec.Containers[i]
			break
		}
	}
	if app == nil && len(podSpec.Containers) > 0 {
		app = &podSpec.Containers[0]
	}
	if app != nil && app.SecurityContext != nil {
		if app.SecurityContext.RunAsUser != nil {
			user = app.SecurityContext.RunAsUser
		}
		if app.SecurityContext.RunAsGroup != nil {
			group = app.SecurityContext.RunAsGroup
		}
	}
	return user, group
}

// nfsSeedScript copies the NFS export into the temp dir and rewrites preview URLs. Its inputs
// arrive as environment variables, so no value is interpolated into shell source.
const nfsSeedScript = `set -u
fail() { echo "config-seed: $*" >&2; exit 1; }
SRC="${SRC_DIR:-/src}"
DST="${CONFIG_DIR:-/config}"
find "$DST" -mindepth 1 -delete || fail "cannot clear $DST"
SRC_KIB=$(du -sk "$SRC" | cut -f1) || fail "cannot read $SOURCE; the export may be root-squashed; set nfs.copy: false to start empty"
if [ "$SRC_KIB" -gt "$LIMIT_KIB" ]; then
  fail "source $SOURCE is $((SRC_KIB / 1024))Mi, over sizeLimit $LIMIT; raise configVolume.nfs.sizeLimit or set nfs.copy: false"
fi
cp -a "$SRC"/. "$DST"/ || fail "cannot copy $SOURCE; the export may be root-squashed; set nfs.copy: false to start empty"
export PROD_NS PREVIEW_NS DOMAIN PR APP
find "$DST" -type f -exec sh -c '
  for file in "$@"; do
    grep -Iq . "$file"
    status=$?
    if [ "$status" -eq 1 ]; then continue; fi
    if [ "$status" -ge 2 ]; then
      echo "config-seed: cannot read $file; the export may be root-squashed; set nfs.copy: false to start empty" >&2
      exit 1
    fi
    sed -i "s/\.${PROD_NS}\.svc\.cluster\.local/.${PREVIEW_NS}.svc.cluster.local/g" "$file" || exit 1
    sed -i "s/${APP}\.${DOMAIN}/pr-${PR}-${APP}.${DOMAIN}/g" "$file" || exit 1
  done
' sh {} + || fail "URL rewrite failed"
if [ -n "${SEED_UID:-}" ]; then
  chown -R "$SEED_UID:${SEED_GID:-$SEED_UID}" "$DST" || fail "cannot chown $DST"
else
  chmod -R a+rwX "$DST" || fail "cannot chmod $DST"
fi
echo "config-seed: seeded from $SOURCE"
`
