package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// cnpgClone fully specifies a snapshot-based CNPG clone. Callers pass explicit
// resource names so existing flows (preview, keyed by PR number) keep their
// names while new flows (MigrationCheck, keyed by CR UID) get collision-free
// names that survive reruns / force-pushes.
type cnpgClone struct {
	sourceCluster    string // production CNPG Cluster name
	sourceClusterNS  string // production CNPG Cluster namespace (e.g. "postgres")
	targetNS         string // namespace where the clone Cluster + restore VolumeSnapshot are created
	prodSnapshotName string // on-demand VolumeSnapshot created in sourceClusterNS
	vscName          string // pre-provisioned VolumeSnapshotContent (cluster-scoped)
	vsName           string // restore VolumeSnapshot created in targetNS
	clusterName      string // clone CNPG Cluster name in targetNS
	snapshotClass    string // VolumeSnapshotClass (default "ceph-block-snapshot")
	labels           map[string]string
}

// cloneCNPGFromSnapshot snapshots the source cluster's primary PVC and bootstraps
// a single-instance CNPG clone from it in targetNS. The clone's Postgres image is
// copied from the source cluster so a restored data directory is read by a
// matching major version (never assume PG 17). The pre-provisioned
// VolumeSnapshotContent uses deletionPolicy: Retain; reclaiming the underlying
// CSI snapshot is the caller's responsibility (delete the production
// VolumeSnapshot at teardown).
func (h *PreviewHandler) cloneCNPGFromSnapshot(ctx context.Context, c cnpgClone) error {
	if c.snapshotClass == "" {
		c.snapshotClass = "ceph-block-snapshot"
	}

	// --- Find the primary PVC + source image from the production CNPG cluster ---
	prodCluster := &unstructured.Unstructured{}
	prodCluster.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"})
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: c.sourceClusterNS, Name: c.sourceCluster}, prodCluster); err != nil {
		return fmt.Errorf("failed to get source CNPG cluster %s/%s: %w", c.sourceClusterNS, c.sourceCluster, err)
	}
	primaryPod, _, _ := unstructured.NestedString(prodCluster.Object, "status", "currentPrimary")
	if primaryPod == "" {
		return fmt.Errorf("source CNPG cluster %s has no currentPrimary", c.sourceCluster)
	}
	primaryPVC := primaryPod // CNPG PVC name == pod name
	// Copy the major version source (imageName and/or imageCatalogRef) so the
	// restored data dir is read by a compatible binary.
	sourceImage, _, _ := unstructured.NestedString(prodCluster.Object, "spec", "imageName")
	sourceCatalogRef, _, _ := unstructured.NestedMap(prodCluster.Object, "spec", "imageCatalogRef")

	// --- Create on-demand VolumeSnapshot in the source (postgres) namespace ---
	prodSnapshot := newVolumeSnapshot(c.sourceClusterNS, c.prodSnapshotName, c.labels)
	if err := unstructured.SetNestedMap(prodSnapshot.Object, map[string]interface{}{
		"volumeSnapshotClassName": c.snapshotClass,
		"source":                  map[string]interface{}{"persistentVolumeClaimName": primaryPVC},
	}, "spec"); err != nil {
		return err
	}
	if err := h.createOrUpdate(ctx, prodSnapshot); err != nil {
		return fmt.Errorf("failed to create production VolumeSnapshot: %w", err)
	}
	if err := h.waitForSnapshotReady(ctx, c.sourceClusterNS, c.prodSnapshotName); err != nil {
		return fmt.Errorf("production database snapshot not ready: %w", err)
	}

	// --- Read snapshot metadata (handle, driver, restoreSize) ---
	prodSnapshot = newVolumeSnapshot(c.sourceClusterNS, c.prodSnapshotName, nil)
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: c.sourceClusterNS, Name: c.prodSnapshotName}, prodSnapshot); err != nil {
		return fmt.Errorf("failed to re-read production snapshot: %w", err)
	}
	boundVSCName, _, _ := unstructured.NestedString(prodSnapshot.Object, "status", "boundVolumeSnapshotContentName")
	if boundVSCName == "" {
		return fmt.Errorf("production snapshot has no boundVolumeSnapshotContentName")
	}
	boundVSC := &unstructured.Unstructured{}
	boundVSC.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
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
		restoreSize = "10Gi"
	}
	h.log.Info("Got database snapshot metadata", "handle", snapshotHandle, "driver", driver, "restoreSize", restoreSize)

	// --- Pre-provisioned VolumeSnapshotContent (cluster-scoped) + restore VolumeSnapshot in targetNS ---
	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	vsc.SetName(c.vscName)
	vsc.SetLabels(c.labels)
	if err := unstructured.SetNestedMap(vsc.Object, map[string]interface{}{
		"driver":                  driver,
		"deletionPolicy":          "Retain",
		"source":                  map[string]interface{}{"snapshotHandle": snapshotHandle},
		"volumeSnapshotClassName": c.snapshotClass,
		"volumeSnapshotRef":       map[string]interface{}{"name": c.vsName, "namespace": c.targetNS},
	}, "spec"); err != nil {
		return err
	}
	if err := h.createOrUpdate(ctx, vsc); err != nil {
		return fmt.Errorf("failed to create VolumeSnapshotContent: %w", err)
	}

	vs := newVolumeSnapshot(c.targetNS, c.vsName, c.labels)
	if err := unstructured.SetNestedMap(vs.Object, map[string]interface{}{
		"source": map[string]interface{}{"volumeSnapshotContentName": c.vscName},
	}, "spec"); err != nil {
		return err
	}
	if err := h.createOrUpdate(ctx, vs); err != nil {
		return fmt.Errorf("failed to create VolumeSnapshot: %w", err)
	}

	// --- Clone CNPG Cluster bootstrapped from the restore snapshot ---
	cnpgSpec := map[string]interface{}{
		"instances": int64(1),
		"inheritedMetadata": map[string]interface{}{
			"labels": map[string]interface{}{"istio.io/dataplane-mode": "none"},
		},
		"bootstrap": map[string]interface{}{
			"recovery": map[string]interface{}{
				"volumeSnapshots": map[string]interface{}{
					"storage": map[string]interface{}{
						"name":     c.vsName,
						"kind":     "VolumeSnapshot",
						"apiGroup": "snapshot.storage.k8s.io",
					},
				},
			},
		},
		"postgresql": map[string]interface{}{
			"shared_preload_libraries": []interface{}{"pg_stat_statements"},
			"parameters":               map[string]interface{}{"shared_buffers": "128MB", "max_connections": "50"},
		},
		"storage": map[string]interface{}{"size": restoreSize, "storageClass": "rook-ceph-block"},
		"resources": map[string]interface{}{
			"requests": map[string]interface{}{"memory": "256Mi", "cpu": "100m"},
			"limits":   map[string]interface{}{"memory": "512Mi", "cpu": "500m"},
		},
		"enableSuperuserAccess": true,
	}
	// Match the source Postgres major version (#4): prefer imageName, else imageCatalogRef.
	switch {
	case sourceImage != "":
		cnpgSpec["imageName"] = sourceImage
	case sourceCatalogRef != nil:
		cnpgSpec["imageCatalogRef"] = sourceCatalogRef
	default:
		h.log.Info("source cluster has neither imageName nor imageCatalogRef; CNPG will use its default image", "cluster", c.sourceCluster)
	}

	cnpgCluster := &unstructured.Unstructured{}
	cnpgCluster.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"})
	cnpgCluster.SetNamespace(c.targetNS)
	cnpgCluster.SetName(c.clusterName)
	cnpgCluster.SetLabels(c.labels)
	if err := unstructured.SetNestedMap(cnpgCluster.Object, cnpgSpec, "spec"); err != nil {
		return err
	}
	if err := h.createOrUpdate(ctx, cnpgCluster); err != nil {
		return fmt.Errorf("failed to create CNPG Cluster: %w", err)
	}

	h.log.Info("Created CNPG clone from snapshot", "cluster", c.clusterName, "namespace", c.targetNS)
	return nil
}

// newVolumeSnapshot returns an unstructured VolumeSnapshot scaffold.
func newVolumeSnapshot(namespace, name string, labels map[string]string) *unstructured.Unstructured {
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"})
	if namespace != "" {
		vs.SetNamespace(namespace)
	}
	vs.SetName(name)
	if labels != nil {
		vs.SetLabels(labels)
	}
	return vs
}
