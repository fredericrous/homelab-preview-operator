package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	// warmSnapshotName opts this clone into reusing a long-lived "warm" snapshot
	// of the source primary PVC (shared across runs) instead of creating a fresh
	// on-demand snapshot in prodSnapshotName. When empty (e.g. the preview flow),
	// behaviour is unchanged: a per-run snapshot is created and waited on.
	warmSnapshotName string
	// warmMaxAge is the freshness ceiling for reuse: a warm snapshot older than
	// this is refreshed before use. Ignored unless warmSnapshotName is set.
	warmMaxAge time.Duration
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
	// Match the source storage class — restoring an encrypted Ceph snapshot into a
	// volume of a different (e.g. unencrypted) class fails: "cannot create
	// unencrypted volume from encrypted volume".
	sourceStorageClass, _, _ := unstructured.NestedString(prodCluster.Object, "spec", "storage", "storageClass")
	if sourceStorageClass == "" {
		sourceStorageClass = "rook-ceph-block"
	}

	// --- Acquire a ready source snapshot (warm-reuse aware) + its CSI metadata ---
	snapshotHandle, driver, restoreSize, err := h.acquireSourceSnapshot(ctx, c, primaryPVC)
	if err != nil {
		return err
	}

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
		"storage": map[string]interface{}{"size": restoreSize, "storageClass": sourceStorageClass},
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

// acquireSourceSnapshot returns a ready CSI snapshot of the source primary PVC
// to restore from, together with its bound VolumeSnapshotContent's snapshotHandle
// + driver (so the caller can build a pre-provisioned restore VSC in the
// throwaway namespace) and the restoreSize.
//
// When c.warmSnapshotName is set it reuses a long-lived "warm" snapshot, lifting
// the snapshot create+wait off the request's critical path: a settled snapshot is
// already readyToUse and has had time for Ceph to flatten it, so the restore clone
// materialises faster too. Only a cold start (no snapshot yet) or a snapshot past
// warmMaxAge pays the create cost; every check in between restores instantly.
// Per-run restore VSCs use deletionPolicy: Retain, so tearing an individual check
// down never reclaims the shared warm snapshot's underlying CSI object.
//
// With warmSnapshotName empty the behaviour is unchanged: a per-run snapshot named
// prodSnapshotName is created and waited on (the preview flow relies on this).
func (h *PreviewHandler) acquireSourceSnapshot(ctx context.Context, c cnpgClone, primaryPVC string) (snapshotHandle, driver, restoreSize string, err error) {
	name := c.prodSnapshotName
	warm := c.warmSnapshotName != ""
	if warm {
		name = c.warmSnapshotName
	}

	reuse := false
	if warm {
		existing := newVolumeSnapshot(c.sourceClusterNS, name, nil)
		getErr := h.client.Get(ctx, types.NamespacedName{Namespace: c.sourceClusterNS, Name: name}, existing)
		switch {
		case getErr == nil:
			ready, _, _ := unstructured.NestedBool(existing.Object, "status", "readyToUse")
			age := time.Since(existing.GetCreationTimestamp().Time)
			switch {
			case warmSnapshotUsable(ready, age, c.warmMaxAge):
				reuse = true
				h.log.Info("reusing warm database snapshot", "name", name, "ageSeconds", int(age.Seconds()))
			case age >= c.warmMaxAge:
				// Past the freshness ceiling — replace it so migrations run against
				// reasonably-current data. Deleting reclaims the old CSI snapshot
				// (class deletionPolicy: Delete); recreate below under the same name.
				h.log.Info("warm snapshot stale, refreshing", "name", name, "ageSeconds", int(age.Seconds()))
				if derr := h.deleteSnapshotAndWaitGone(ctx, c.sourceClusterNS, name); derr != nil {
					return "", "", "", fmt.Errorf("refresh stale warm snapshot: %w", derr)
				}
			default:
				// Exists but not yet readyToUse and still within maxAge — a create
				// is already in flight (a concurrent check). Fall through and wait
				// on the existing object rather than fighting over it.
				h.log.Info("warm snapshot exists but not ready yet, waiting", "name", name)
			}
		case errors.IsNotFound(getErr):
			// Cold start — create below.
		default:
			return "", "", "", fmt.Errorf("get warm snapshot %s/%s: %w", c.sourceClusterNS, name, getErr)
		}
	}

	if !reuse {
		snap := newVolumeSnapshot(c.sourceClusterNS, name, c.labels)
		if err := unstructured.SetNestedMap(snap.Object, map[string]interface{}{
			"volumeSnapshotClassName": c.snapshotClass,
			"source":                  map[string]interface{}{"persistentVolumeClaimName": primaryPVC},
		}, "spec"); err != nil {
			return "", "", "", err
		}
		// A VolumeSnapshot's source is immutable, and concurrent checks may race to
		// create the same warm snapshot: create-if-absent, tolerate AlreadyExists,
		// then wait — never Update (which would reject the immutable spec).
		if err := h.client.Create(ctx, snap); err != nil && !errors.IsAlreadyExists(err) {
			return "", "", "", fmt.Errorf("create source VolumeSnapshot %s/%s: %w", c.sourceClusterNS, name, err)
		}
		if err := h.waitForSnapshotReady(ctx, c.sourceClusterNS, name); err != nil {
			return "", "", "", fmt.Errorf("source database snapshot not ready: %w", err)
		}
	}

	// Read handle/driver/restoreSize from the (now-ready) snapshot's bound VSC.
	snap := newVolumeSnapshot(c.sourceClusterNS, name, nil)
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: c.sourceClusterNS, Name: name}, snap); err != nil {
		return "", "", "", fmt.Errorf("re-read source snapshot %s/%s: %w", c.sourceClusterNS, name, err)
	}
	boundVSCName, _, _ := unstructured.NestedString(snap.Object, "status", "boundVolumeSnapshotContentName")
	if boundVSCName == "" {
		return "", "", "", fmt.Errorf("source snapshot %s has no boundVolumeSnapshotContentName", name)
	}
	boundVSC := &unstructured.Unstructured{}
	boundVSC.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	if err := h.client.Get(ctx, types.NamespacedName{Name: boundVSCName}, boundVSC); err != nil {
		return "", "", "", fmt.Errorf("get bound VolumeSnapshotContent: %w", err)
	}
	snapshotHandle, _, _ = unstructured.NestedString(boundVSC.Object, "status", "snapshotHandle")
	if snapshotHandle == "" {
		return "", "", "", fmt.Errorf("bound VolumeSnapshotContent has no snapshotHandle")
	}
	driver, _, _ = unstructured.NestedString(boundVSC.Object, "spec", "driver")
	restoreSize, _, _ = unstructured.NestedString(snap.Object, "status", "restoreSize")
	if restoreSize == "" {
		restoreSize = "10Gi"
	}
	h.log.Info("source snapshot metadata", "handle", snapshotHandle, "driver", driver, "restoreSize", restoreSize, "warm", warm, "reused", reuse)
	return snapshotHandle, driver, restoreSize, nil
}

// warmSnapshotUsable reports whether a warm snapshot can be restored from as-is:
// it must be readyToUse and younger than the freshness ceiling.
func warmSnapshotUsable(ready bool, age, maxAge time.Duration) bool {
	return ready && age < maxAge
}

// deleteSnapshotAndWaitGone deletes a VolumeSnapshot and blocks until the API no
// longer returns it, so an immediate recreate under the same name won't race a
// pending deletion.
func (h *PreviewHandler) deleteSnapshotAndWaitGone(ctx context.Context, namespace, name string) error {
	vs := newVolumeSnapshot(namespace, name, nil)
	if err := client.IgnoreNotFound(h.client.Delete(ctx, vs)); err != nil {
		return err
	}
	timeout := time.After(60 * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timed out waiting for VolumeSnapshot %s/%s to delete", namespace, name)
		case <-ticker.C:
			probe := newVolumeSnapshot(namespace, name, nil)
			err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, probe)
			if errors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
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
