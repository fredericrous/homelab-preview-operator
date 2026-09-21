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
	// of the source database (shared across runs) instead of creating a fresh
	// on-demand snapshot in prodSnapshotName. When empty (e.g. the preview flow),
	// behaviour is unchanged: a per-run snapshot is created and waited on.
	//
	// A warm snapshot is taken as a CNPG cold Backup of a STANDBY whenever the
	// source cluster has one (see coldSnapshotEligible), and falls back to a
	// crash-consistent snapshot of the primary PVC otherwise.
	warmSnapshotName string
	// warmMaxAge is the freshness ceiling for reuse: a warm snapshot older than
	// this is refreshed before use. Ignored unless warmSnapshotName is set.
	warmMaxAge time.Duration
}

// coldBackupTimeout bounds the wait for a CNPG cold Backup to reach `completed`.
// A cold backup fences the target instance, snapshots its PVC and unfences it;
// on this cluster that is well under a minute, and the 120 s the raw snapshot
// path allows itself is too tight once the fence/unfence round trip is added.
const coldBackupTimeout = 5 * time.Minute

// cloneCNPGFromSnapshot snapshots the source cluster and bootstraps a
// single-instance CNPG clone from it in targetNS. The clone's Postgres image is
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

	// A warm snapshot is a cold Backup of a standby when the source has one; the
	// decision is made from the live Cluster status so a source that lost its
	// standby degrades to the raw path instead of fencing its primary.
	cold, coldReason := false, "per-run snapshot"
	if c.warmSnapshotName != "" {
		cold, coldReason = coldSnapshotEligible(prodCluster)
	}

	// --- Acquire a ready source snapshot (warm-reuse aware) + its CSI metadata ---
	snapshotHandle, driver, restoreSize, err := h.acquireSourceSnapshot(ctx, c, primaryPVC, cold, coldReason)
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
	cnpgSpec := buildCloneClusterSpec(c.vsName, restoreSize, sourceStorageClass, sourceImage, sourceCatalogRef)
	if sourceImage == "" && sourceCatalogRef == nil {
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

// coldSnapshotEligible reports whether the source cluster can give a warm
// snapshot as a CNPG cold (offline) volume-snapshot Backup of a standby, and
// why not otherwise.
//
// Why a cold backup, and why of a standby: a raw snapshot of the primary's PVC is
// crash-consistent, so the clone must replay every WAL record since the
// primary's last checkpoint, and the end-of-recovery checkpoint then rewrites
// every page that replay dirtied. On a Ceph clone-from-snapshot each first write
// to a 4 MiB object is a copy-up from the parent, and krbd drains those at well
// under one object per second here — so a source with busy writers (litellm's
// spend log, blocky's query log) turned a 3-minute bring-up into 20+ minutes
// between 2026-09-12 and 2026-09-21, and the migration check's budget ran out.
// A cold backup fences the instance first: the data directory is a clean
// shutdown, there is nothing to replay and nothing to rewrite, and the clone is
// accepting connections in seconds. Fencing the primary would take production
// down for the snapshot; a standby costs nothing to fence, which is what the
// second instance buys.
//
// Refused when there is no ready standby (CNPG's prefer-standby would silently
// pick the primary), and when synchronous replication is configured without
// dataDurability=preferred: fencing the only standby under `required` blocks
// every commit on the primary until the snapshot is done. legacy
// minSyncReplicas is treated the same way.
func coldSnapshotEligible(cluster *unstructured.Unstructured) (bool, string) {
	instances, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances")
	ready, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
	if instances < 2 || ready < 2 {
		return false, fmt.Sprintf("no ready standby to fence (instances=%d readyInstances=%d); taking a crash-consistent snapshot of the primary instead", instances, ready)
	}
	if minSync, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "minSyncReplicas"); minSync > 0 {
		return false, fmt.Sprintf("minSyncReplicas=%d would block commits while the standby is fenced", minSync)
	}
	if sync, found, _ := unstructured.NestedMap(cluster.Object, "spec", "postgresql", "synchronous"); found && sync != nil {
		// CNPG defaults dataDurability to "required"; only an explicit "preferred"
		// lets the primary carry on alone.
		if durability, _ := sync["dataDurability"].(string); durability != "preferred" {
			return false, fmt.Sprintf("synchronous replication with dataDurability=%q would block commits while the standby is fenced", durability)
		}
	}
	return true, "cold backup of a standby"
}

// buildColdBackup returns the CNPG Backup that snapshots a standby offline.
// Pure so the request is assertable without a live cluster.
//
// CNPG names the resulting PGDATA VolumeSnapshot after the Backup, so naming the
// Backup after the warm snapshot keeps acquireSourceSnapshot's reuse logic
// unchanged: it watches the VolumeSnapshot of that name regardless of how it was
// produced. `online: false` overrides the source cluster's default (this cluster
// runs online snapshot backups for its own purposes); `target: prefer-standby`
// is only honoured once coldSnapshotEligible has confirmed a standby exists.
func buildColdBackup(namespace, name, cluster string, labels map[string]string) *unstructured.Unstructured {
	b := &unstructured.Unstructured{}
	b.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Backup"})
	b.SetNamespace(namespace)
	b.SetName(name)
	if labels != nil {
		b.SetLabels(labels)
	}
	b.Object["spec"] = map[string]interface{}{
		"cluster": map[string]interface{}{"name": cluster},
		"method":  "volumeSnapshot",
		"online":  false,
		"target":  "prefer-standby",
	}
	return b
}

// acquireSourceSnapshot returns a ready CSI snapshot of the source database to
// restore from, together with its bound VolumeSnapshotContent's snapshotHandle
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
// A warm snapshot is produced by a CNPG cold Backup of a standby when `cold` is
// set (see coldSnapshotEligible), and by a raw VolumeSnapshot of primaryPVC
// otherwise. With warmSnapshotName empty the behaviour is unchanged: a per-run
// raw snapshot named prodSnapshotName is created and waited on (the preview flow
// relies on this).
func (h *PreviewHandler) acquireSourceSnapshot(ctx context.Context, c cnpgClone, primaryPVC string, cold bool, coldReason string) (snapshotHandle, driver, restoreSize string, err error) {
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
				// The Backup that produced it (if any) goes first: CNPG refuses a
				// second Backup of the same name while the first still exists.
				h.log.Info("warm snapshot stale, refreshing", "name", name, "ageSeconds", int(age.Seconds()))
				if derr := h.deleteBackupAndWaitGone(ctx, c.sourceClusterNS, name); derr != nil {
					return "", "", "", fmt.Errorf("refresh stale warm backup: %w", derr)
				}
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
		if warm && cold {
			h.log.Info("taking warm snapshot as a cold backup of a standby", "name", name, "cluster", c.sourceCluster)
			if err := h.ensureColdBackup(ctx, c.sourceClusterNS, name, c.sourceCluster, c.labels); err != nil {
				return "", "", "", err
			}
		} else {
			if warm {
				h.log.Info("taking warm snapshot from the primary PVC", "name", name, "reason", coldReason)
			}
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
	h.log.Info("source snapshot metadata", "handle", snapshotHandle, "driver", driver, "restoreSize", restoreSize, "warm", warm, "reused", reuse, "cold", warm && cold && !reuse)
	return snapshotHandle, driver, restoreSize, nil
}

// ensureColdBackup creates the cold Backup named name (tolerating a concurrent
// creator) and waits for CNPG to report it completed. A leftover Backup of the
// same name that already failed — say the standby was mid-restart — is replaced
// rather than waited on forever, since CNPG never retries a failed Backup.
func (h *PreviewHandler) ensureColdBackup(ctx context.Context, namespace, name, cluster string, labels map[string]string) error {
	existing := newBackup(namespace, name)
	switch getErr := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, existing); {
	case getErr == nil:
		if phase, _, _ := unstructured.NestedString(existing.Object, "status", "phase"); phase == "failed" {
			reason, _, _ := unstructured.NestedString(existing.Object, "status", "error")
			h.log.Info("replacing failed warm backup", "name", name, "error", reason)
			if err := h.deleteBackupAndWaitGone(ctx, namespace, name); err != nil {
				return fmt.Errorf("replace failed warm backup: %w", err)
			}
		}
	case errors.IsNotFound(getErr):
	default:
		return fmt.Errorf("get warm backup %s/%s: %w", namespace, name, getErr)
	}
	if err := h.client.Create(ctx, buildColdBackup(namespace, name, cluster, labels)); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create cold Backup %s/%s: %w", namespace, name, err)
	}
	return h.waitForBackupCompleted(ctx, namespace, name)
}

// waitForBackupCompleted blocks until the Backup's phase is `completed`, fails
// on `failed` with CNPG's own error text, and gives up after coldBackupTimeout.
func (h *PreviewHandler) waitForBackupCompleted(ctx context.Context, namespace, name string) error {
	timeout := time.After(coldBackupTimeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timed out waiting for Backup %s/%s to complete", namespace, name)
		case <-ticker.C:
			b := newBackup(namespace, name)
			if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, b); err != nil {
				if errors.IsNotFound(err) {
					continue
				}
				return err
			}
			switch phase, _, _ := unstructured.NestedString(b.Object, "status", "phase"); phase {
			case "completed":
				return nil
			case "failed":
				reason, _, _ := unstructured.NestedString(b.Object, "status", "error")
				return fmt.Errorf("Backup %s/%s failed: %s", namespace, name, reason)
			}
		}
	}
}

// deleteBackupAndWaitGone deletes a CNPG Backup (no-op when absent) and blocks
// until the API no longer returns it, so an immediate recreate under the same
// name won't race a pending deletion.
func (h *PreviewHandler) deleteBackupAndWaitGone(ctx context.Context, namespace, name string) error {
	if err := client.IgnoreNotFound(h.client.Delete(ctx, newBackup(namespace, name))); err != nil {
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
			return fmt.Errorf("timed out waiting for Backup %s/%s to delete", namespace, name)
		case <-ticker.C:
			err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, newBackup(namespace, name))
			if errors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
}

// newBackup returns an unstructured CNPG Backup scaffold.
func newBackup(namespace, name string) *unstructured.Unstructured {
	b := &unstructured.Unstructured{}
	b.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Backup"})
	b.SetNamespace(namespace)
	b.SetName(name)
	return b
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

// buildCloneClusterSpec returns the CNPG Cluster spec for a snapshot-restored
// clone. Pure so the tuning that keeps bring-up under the consumer's deadline
// (resources, durability, and OSD-local placement) is assertable without a live
// cluster.
func buildCloneClusterSpec(vsName, restoreSize, storageClass, sourceImage string, sourceCatalogRef interface{}) map[string]interface{} {
	cnpgSpec := map[string]interface{}{
		"instances": int64(1),
		"inheritedMetadata": map[string]interface{}{
			"labels": map[string]interface{}{"istio.io/dataplane-mode": "none"},
		},
		"bootstrap": map[string]interface{}{
			"recovery": map[string]interface{}{
				"volumeSnapshots": map[string]interface{}{
					"storage": map[string]interface{}{
						"name":     vsName,
						"kind":     "VolumeSnapshot",
						"apiGroup": "snapshot.storage.k8s.io",
					},
				},
			},
		},
		"postgresql": map[string]interface{}{
			"shared_preload_libraries": []interface{}{"pg_stat_statements"},
			"parameters": map[string]interface{}{
				"shared_buffers":  "512MB",
				"max_connections": "50",
				// The clone is disposable and its volume is a copy-on-write child of
				// the source snapshot, where every first write to a 4 MiB object is a
				// copy-up that krbd drains at under one per second on this cluster.
				// Durability buys nothing here — a crashed clone is torn down, never
				// recovered — so let Postgres hand writes to the page cache and move
				// on rather than fsync each checkpointed file through that path.
				"fsync":              "off",
				"synchronous_commit": "off",
				"full_page_writes":   "off",
			},
		},
		"storage": map[string]interface{}{"size": restoreSize, "storageClass": storageClass},
		// The clone is short-lived but its bring-up is latency-critical: the
		// consumer (e.g. the migration-check CI job) waits on a deadline, and a
		// clone that boots too slowly is a false failure. Snapshot restore +
		// crash-recovery + first-connection is CPU- and IO-heavy, so a 500m cap
		// throttled startup badly (observed ~6min to accept connections against
		// a multi-DB source). Give it real headroom — it lives for minutes and
		// the extra request is reclaimed at teardown.
		"resources": map[string]interface{}{
			"requests": map[string]interface{}{"memory": "512Mi", "cpu": "500m"},
			"limits":   map[string]interface{}{"memory": "2Gi", "cpu": "4"},
		},
		// Bring-up is IO-bound, and CPU headroom alone did not fix it: postgres
		// replays WAL off an encrypted (LUKS) RBD snapshot, so every read served
		// from a remote OSD costs a network hop plus decrypt. Land the clone on
		// a node that hosts an OSD and those reads stay local.
		//
		// Affinity to the OSD pods rather than a node label or node name: the
		// set of OSD-hosting nodes is not labelled distinctly, and hardcoding
		// hostnames rots the moment the cluster is rebalanced. Preferred, not
		// required — if no OSD node can take the pod we want a slow clone, not
		// an unschedulable one. `namespaces` is mandatory here: pod affinity
		// defaults to the scheduled pod's own namespace, which is the throwaway
		// clone namespace, where no OSD will ever run.
		"affinity": map[string]interface{}{
			"additionalPodAffinity": map[string]interface{}{
				"preferredDuringSchedulingIgnoredDuringExecution": []interface{}{
					map[string]interface{}{
						"weight": int64(100),
						"podAffinityTerm": map[string]interface{}{
							"topologyKey": "kubernetes.io/hostname",
							"namespaces":  []interface{}{"rook-ceph"},
							"labelSelector": map[string]interface{}{
								"matchLabels": map[string]interface{}{"app": "rook-ceph-osd"},
							},
						},
					},
				},
			},
		},
		"enableSuperuserAccess": true,
	}
	// Match the source Postgres major version (#4): prefer imageName, else imageCatalogRef.
	switch {
	case sourceImage != "":
		cnpgSpec["imageName"] = sourceImage
	case sourceCatalogRef != nil:
		cnpgSpec["imageCatalogRef"] = sourceCatalogRef
	}
	return cnpgSpec
}
