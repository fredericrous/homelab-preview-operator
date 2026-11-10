package controller

import (
	"context"
	stderrors "errors"
	"fmt"

	kustomize "github.com/fluxcd/pkg/apis/kustomize"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	sigsyaml "sigs.k8s.io/yaml"

	v1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

// errClaimNotBound reports a PVC that names no PersistentVolume yet.
var errClaimNotBound = stderrors.New("claim is not bound to a PV")

// nfsMount is one production Deployment volume that claims an NFS-backed PVC.
type nfsMount struct {
	VolumeName string
	Server     string
	Path       string
}

// readBoundPV returns the PersistentVolume a claim is bound to, or errClaimNotBound.
func (h *PreviewHandler) readBoundPV(ctx context.Context, namespace, claimName string) (*corev1.PersistentVolume, error) {
	var claim corev1.PersistentVolumeClaim
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: claimName}, &claim); err != nil {
		return nil, fmt.Errorf("cannot read PVC in namespace %q: %w", namespace, err)
	}
	if claim.Spec.VolumeName == "" {
		return nil, errClaimNotBound
	}
	var volume corev1.PersistentVolume
	if err := h.client.Get(ctx, types.NamespacedName{Name: claim.Spec.VolumeName}, &volume); err != nil {
		return nil, fmt.Errorf("cannot read PV %q: %w", claim.Spec.VolumeName, err)
	}
	return &volume, nil
}

// resolveNFSVolumes reads the cluster state nfsVolumes needs before patches are built.
// It returns nil when the config has no nfsVolumes.
func (h *PreviewHandler) resolveNFSVolumes(ctx context.Context, appName string, config *v1.PreviewConfig) ([]nfsMount, error) {
	if len(config.Spec.NFSVolumes) == 0 {
		return nil, nil
	}
	if config.Spec.DeploymentType == v1.DeploymentTypeHelm {
		return nil, fmt.Errorf("nfsVolumes supports raw-manifest Deployments only; Helm-rendered Deployments are not patched")
	}
	cv := config.Spec.ConfigVolume
	for _, entry := range config.Spec.NFSVolumes {
		if cv != nil && cv.ClaimName == entry.ClaimName {
			return nil, fmt.Errorf("claim %q is in both configVolume.claimName and nfsVolumes; use one", entry.ClaimName)
		}
	}

	var deployment appsv1.Deployment
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: appName, Name: appName}, &deployment); err != nil {
		return nil, fmt.Errorf("nfsVolumes: cannot read production Deployment %q in namespace %q: %w", appName, appName, err)
	}

	var mounts []nfsMount
	seen := map[string]bool{}
	for _, entry := range config.Spec.NFSVolumes {
		if seen[entry.ClaimName] {
			continue
		}
		seen[entry.ClaimName] = true

		var volumeNames []string
		for _, volume := range deployment.Spec.Template.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == entry.ClaimName {
				volumeNames = append(volumeNames, volume.Name)
			}
		}
		if len(volumeNames) == 0 {
			return nil, fmt.Errorf("nfsVolumes claim %q is not used as a persistentVolumeClaim volume by Deployment %q in namespace %q", entry.ClaimName, appName, appName)
		}
		volume, err := h.readBoundPV(ctx, appName, entry.ClaimName)
		if stderrors.Is(err, errClaimNotBound) {
			return nil, fmt.Errorf("nfsVolumes claim %q in namespace %q is not bound to a PV", entry.ClaimName, appName)
		}
		if err != nil {
			return nil, fmt.Errorf("nfsVolumes claim %q: %w", entry.ClaimName, err)
		}
		if volume.Spec.NFS == nil {
			return nil, fmt.Errorf("nfsVolumes claim %q is bound to PV %q, which has no spec.nfs", entry.ClaimName, volume.Name)
		}
		for _, name := range volumeNames {
			mounts = append(mounts, nfsMount{VolumeName: name, Server: volume.Spec.NFS.Server, Path: volume.Spec.NFS.Path})
		}
	}
	return mounts, nil
}

// generateNFSVolumesPatch replaces each claimed volume with an inline read-only nfs volume.
// nfs.readOnly makes the kubelet mount the export read-only for every container mount.
func generateNFSVolumesPatch(appName string, mounts []nfsMount) (*kustomize.Patch, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	volumes := make([]map[string]interface{}, 0, len(mounts))
	for _, mount := range mounts {
		volumes = append(volumes, map[string]interface{}{
			"name":                  mount.VolumeName,
			"persistentVolumeClaim": nil,
			"nfs":                   map[string]interface{}{"server": mount.Server, "path": mount.Path, "readOnly": true},
		})
	}
	doc := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": appName},
		"spec":       map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"volumes": volumes}}},
	}
	out, err := sigsyaml.Marshal(doc)
	if err != nil {
		// A dropped patch would leave the production NFS claim mounted read-write.
		return nil, fmt.Errorf("render the nfsVolumes patch for Deployment %q: %w", appName, err)
	}
	return &kustomize.Patch{
		Target: &kustomize.Selector{Kind: "Deployment", Name: appName},
		Patch:  string(out),
	}, nil
}

// generateNFSClaimStripPatches deletes the rendered PVC of each nfsVolumes claim.
func generateNFSClaimStripPatches(volumes []v1.NFSVolume) []kustomize.Patch {
	patches := make([]kustomize.Patch, 0, len(volumes))
	for _, entry := range volumes {
		patches = append(patches, kustomize.Patch{
			Target: &kustomize.Selector{Kind: "PersistentVolumeClaim", Name: entry.ClaimName},
			Patch:  fmt.Sprintf("apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: %s\n$patch: delete", entry.ClaimName),
		})
	}
	return patches
}
