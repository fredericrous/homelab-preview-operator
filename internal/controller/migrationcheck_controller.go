package controller

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	previewv1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

const migrationCheckFinalizer = "preview.homelab.io/migration-check"

// warmSnapshotMaxAge is how fresh a shared "warm" source snapshot must be to be
// reused by a MigrationCheck. Migration checks validate that migrations apply
// against real data shapes, for which data a few hours old is fine; a generous
// ceiling means most checks in a burst of PR activity restore instantly, while
// the snapshot is still refreshed often enough to stay representative. The first
// check past this window pays the one-off snapshot cost, then it's warm again.
const warmSnapshotMaxAge = 12 * time.Hour

// MigrationCheckReconciler provisions a throwaway, snapshot-based CNPG clone of a
// production database so CI can run an app's migrations against real data before
// merge, then tears it down on delete / TTL.
type MigrationCheckReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=preview.homelab.io,resources=migrationchecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=preview.homelab.io,resources=migrationchecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=preview.homelab.io,resources=migrationchecks/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch

// Reconcile drives a MigrationCheck through Pending -> Provisioning -> Ready, and
// tears everything down on deletion or TTL expiry.
func (r *MigrationCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("migrationcheck", req.NamespacedName)

	mc := &previewv1.MigrationCheck{}
	if err := r.Get(ctx, req.NamespacedName, mc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: run teardown via finalizer.
	if !mc.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(mc, migrationCheckFinalizer) {
			if err := r.teardown(ctx, log, mc); err != nil {
				log.Error(err, "teardown failed, will retry")
				return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
			}
			controllerutil.RemoveFinalizer(mc, migrationCheckFinalizer)
			if err := r.Update(ctx, mc); err != nil {
				return requeueOnConflict(err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer before creating any external resources.
	if !controllerutil.ContainsFinalizer(mc, migrationCheckFinalizer) {
		controllerutil.AddFinalizer(mc, migrationCheckFinalizer)
		if err := r.Update(ctx, mc); err != nil {
			return requeueOnConflict(err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// TTL guard: if the clone has outlived its TTL, delete the CR (which runs teardown).
	if expired, _ := r.ttlExpired(mc); expired {
		log.Info("MigrationCheck TTL expired, deleting", "name", mc.Name)
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, mc))
	}

	switch mc.Status.Phase {
	case "", previewv1.MigrationCheckPending:
		return r.provision(ctx, log, mc)
	case previewv1.MigrationCheckProvisioning:
		return r.checkReady(ctx, log, mc)
	case previewv1.MigrationCheckReady:
		// Re-queue near TTL expiry so the clone is GC'd even if CI never deletes it.
		_, remaining := r.ttlExpired(mc)
		return ctrl.Result{RequeueAfter: remaining}, nil
	default: // Failed
		return ctrl.Result{}, nil
	}
}

// provision creates the throwaway namespace and kicks off the snapshot clone.
func (r *MigrationCheckReconciler) provision(ctx context.Context, log logr.Logger, mc *previewv1.MigrationCheck) (ctrl.Result, error) {
	srcCluster, srcNS, _, err := r.resolveSource(ctx, mc)
	if err != nil {
		return r.fail(ctx, log, mc, err)
	}

	id := migCheckID(mc)
	cloneNS := fmt.Sprintf("migration-check-%s-%s", mc.Spec.AppName, id)
	labels := map[string]string{
		"preview.homelab.io/migration-check": mc.Name,
		"preview.homelab.io/type":            "migration-check",
	}

	// Throwaway namespace for the clone. Privileged PSS — it only ever holds the
	// operator-managed CNPG clone, and is deleted at teardown.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: cloneNS,
		Labels: map[string]string{
			"preview.homelab.io/migration-check": mc.Name,
			"pod-security.kubernetes.io/enforce": "privileged",
			"istio.io/dataplane-mode":            "none",
			"app.kubernetes.io/managed-by":       "homelab-preview-operator",
		},
	}}
	if err := r.Create(ctx, ns); err != nil && !errors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("create clone namespace: %w", err)
	}

	h := NewPreviewHandler(r.Client, log, "")
	clone := cnpgClone{
		sourceCluster:    srcCluster,
		sourceClusterNS:  srcNS,
		targetNS:         cloneNS,
		prodSnapshotName: fmt.Sprintf("migcheck-%s", id),
		vscName:          fmt.Sprintf("migcheck-%s-content", id),
		vsName:           "migcheck-restore",
		clusterName:      fmt.Sprintf("migcheck-%s", id),
		snapshotClass:    "ceph-block-snapshot",
		labels:           labels,
		// Reuse a shared, long-lived warm snapshot of this source cluster across
		// checks instead of snapshotting the live primary every run — keyed by
		// cluster so every app cloning the same DB shares it.
		warmSnapshotName: fmt.Sprintf("migcheck-warm-%s", srcCluster),
		warmMaxAge:       warmSnapshotMaxAge,
	}
	if err := h.cloneCNPGFromSnapshot(ctx, clone); err != nil {
		return r.fail(ctx, log, mc, fmt.Errorf("clone from snapshot: %w", err))
	}

	expires := mc.CreationTimestamp.Add(r.ttl(mc))
	mc.Status.Phase = previewv1.MigrationCheckProvisioning
	mc.Status.Message = "clone provisioning"
	mc.Status.CloneNamespace = cloneNS
	mc.Status.ExpiresAt = &metav1.Time{Time: expires}
	if err := r.Status().Update(ctx, mc); err != nil {
		return requeueOnConflict(err)
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// checkReady waits until the clone actually accepts connections AND has
// settled, then publishes the DATABASE_URL secret. "Ready" means: the CNPG
// cluster is SETTLED (readyInstances>=1 and phase "Cluster in healthy state" —
// see cnpgClusterSettled for why serving alone is not enough) AND the -rw
// Service has a ready endpoint (CNPG only adds the primary to -rw once its
// readiness probe — a real connection check — passes) AND the superuser secret
// exists. Only then does CI get a connection string (#1).
func (r *MigrationCheckReconciler) checkReady(ctx context.Context, log logr.Logger, mc *previewv1.MigrationCheck) (ctrl.Result, error) {
	id := migCheckID(mc)
	cloneNS := mc.Status.CloneNamespace
	clusterName := fmt.Sprintf("migcheck-%s", id)

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"})
	if err := r.Get(ctx, types.NamespacedName{Namespace: cloneNS, Name: clusterName}, cluster); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	if settled, state := cnpgClusterSettled(cluster); !settled {
		log.V(1).Info("clone not ready yet", "state", state)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// -rw endpoints ready == primary accepting connections.
	eps := &corev1.Endpoints{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cloneNS, Name: clusterName + "-rw"}, eps); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	if !endpointsReady(eps) {
		log.V(1).Info("clone -rw endpoints not ready yet")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Superuser secret (CNPG creates <cluster>-superuser with enableSuperuserAccess).
	suSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cloneNS, Name: clusterName + "-superuser"}, suSecret); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	user := string(suSecret.Data["username"])
	pass := string(suSecret.Data["password"])
	if user == "" || pass == "" {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	dbName := mc.Spec.DatabaseName
	if dbName == "" {
		dbName = mc.Spec.AppName
	}
	dsn := buildDatabaseURL(user, pass, fmt.Sprintf("%s-rw.%s.svc.cluster.local", clusterName, cloneNS), dbName)

	// Publish the result Secret in the CR's namespace (runner-readable). Owner-ref
	// for auto-GC, plus explicit teardown (#5).
	secretName := mc.Name + "-db"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: mc.Namespace,
			Labels:    map[string]string{"preview.homelab.io/migration-check": mc.Name},
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"DATABASE_URL": dsn},
	}
	if err := controllerutil.SetControllerReference(mc, secret, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, secret); err != nil {
		if !errors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create result secret: %w", err)
		}
	}

	mc.Status.Phase = previewv1.MigrationCheckReady
	mc.Status.Message = "clone ready"
	mc.Status.ConnectionSecretName = secretName
	mc.Status.ConnectionSecretNamespace = mc.Namespace
	if err := r.Status().Update(ctx, mc); err != nil {
		return requeueOnConflict(err)
	}
	log.Info("MigrationCheck ready", "secret", secretName, "cloneNamespace", cloneNS)
	_, remaining := r.ttlExpired(mc)
	return ctrl.Result{RequeueAfter: remaining}, nil
}

// teardown removes everything this individual clone created. The throwaway
// namespace deletion reaps the CNPG cluster, PVCs, restore VolumeSnapshot and
// superuser secret; the per-run cluster-scoped VolumeSnapshotContent (Retain, so
// deleting it never touches the underlying CSI snapshot) and the result Secret in
// the CR namespace are deleted explicitly (#5). All deletes are idempotent.
//
// The shared "warm" source snapshot (migcheck-warm-<cluster>) is deliberately NOT
// deleted here — it's reused by subsequent checks and only recycled when a later
// check finds it past warmSnapshotMaxAge. The legacy per-run prod snapshot delete
// below is a no-op under warm reuse (that name is no longer created) and stays for
// back-compat with any in-flight non-warm clone.
func (r *MigrationCheckReconciler) teardown(ctx context.Context, log logr.Logger, mc *previewv1.MigrationCheck) error {
	id := migCheckID(mc)

	// Result secret (CR namespace).
	if mc.Status.ConnectionSecretName != "" {
		_ = client.IgnoreNotFound(r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: mc.Status.ConnectionSecretName, Namespace: mc.Namespace,
		}}))
	}

	// Throwaway namespace (cluster + PVCs + restore VS + superuser secret).
	cloneNS := mc.Status.CloneNamespace
	if cloneNS == "" {
		cloneNS = fmt.Sprintf("migration-check-%s-%s", mc.Spec.AppName, id)
	}
	if err := client.IgnoreNotFound(r.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cloneNS}})); err != nil {
		return fmt.Errorf("delete clone namespace: %w", err)
	}

	// Cluster-scoped VolumeSnapshotContent (Retain — just removes the k8s object).
	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"})
	vsc.SetName(fmt.Sprintf("migcheck-%s-content", id))
	_ = client.IgnoreNotFound(r.Delete(ctx, vsc))

	// Production VolumeSnapshot (Delete policy from its class — reclaims the CSI snapshot).
	_, srcNS, _, err := r.resolveSource(ctx, mc)
	if err != nil {
		srcNS = "postgres" // best-effort if the prod namespace is gone
	}
	prodVS := newVolumeSnapshot(srcNS, fmt.Sprintf("migcheck-%s", id), nil)
	_ = client.IgnoreNotFound(r.Delete(ctx, prodVS))

	log.Info("MigrationCheck torn down", "cloneNamespace", cloneNS)
	return nil
}

// resolveSource determines the source CNPG cluster/namespace and target database.
func (r *MigrationCheckReconciler) resolveSource(ctx context.Context, mc *previewv1.MigrationCheck) (cluster, ns, db string, err error) {
	cluster, ns, db = mc.Spec.ClusterName, mc.Spec.ClusterNamespace, mc.Spec.DatabaseName
	if db == "" {
		db = mc.Spec.AppName
	}
	if cluster == "" || ns == "" {
		prodNS := &corev1.Namespace{}
		if e := r.Get(ctx, types.NamespacedName{Name: mc.Spec.AppName}, prodNS); e != nil {
			return "", "", "", fmt.Errorf("read production namespace %q: %w", mc.Spec.AppName, e)
		}
		if cluster == "" {
			cluster = prodNS.Annotations["postgres.cnpg.io/cluster-name"]
		}
		if ns == "" {
			ns = prodNS.Annotations["postgres.cnpg.io/cluster-namespace"]
			if ns == "" {
				ns = "postgres"
			}
		}
	}
	if cluster == "" {
		return "", "", "", fmt.Errorf("app %q namespace has no postgres.cnpg.io/cluster-name annotation", mc.Spec.AppName)
	}
	return cluster, ns, db, nil
}

func (r *MigrationCheckReconciler) ttl(mc *previewv1.MigrationCheck) time.Duration {
	secs := mc.Spec.TTLSeconds
	if secs <= 0 {
		secs = 3600
	}
	return time.Duration(secs) * time.Second
}

// ttlExpired reports whether the TTL has passed and, if not, the remaining duration.
func (r *MigrationCheckReconciler) ttlExpired(mc *previewv1.MigrationCheck) (bool, time.Duration) {
	deadline := mc.CreationTimestamp.Add(r.ttl(mc))
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return true, 0
	}
	return false, remaining
}

func (r *MigrationCheckReconciler) fail(ctx context.Context, log logr.Logger, mc *previewv1.MigrationCheck, err error) (ctrl.Result, error) {
	log.Error(err, "MigrationCheck failed")
	mc.Status.Phase = previewv1.MigrationCheckFailed
	mc.Status.Message = err.Error()
	if uerr := r.Status().Update(ctx, mc); uerr != nil {
		return requeueOnConflict(uerr)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the controller.
func (r *MigrationCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&previewv1.MigrationCheck{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

// migCheckID returns a stable, DNS-safe id derived from the CR UID (#2 — never
// PR-number, so reruns/force-pushes don't collide).
func migCheckID(mc *previewv1.MigrationCheck) string {
	id := strings.ReplaceAll(string(mc.UID), "-", "")
	if len(id) > 12 {
		id = id[:12]
	}
	if id == "" {
		id = "n" + string(mc.Name) // fallback; UID is always set in-cluster
	}
	return id
}

// buildDatabaseURL builds a libpq URL with userinfo/path properly URL-encoded (#6).
func buildDatabaseURL(user, pass, host, db string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, pass),
		Host:     host + ":5432",
		Path:     "/" + db,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// requeueOnConflict converts an optimistic-concurrency conflict on a
// MigrationCheck write into a quiet immediate requeue. Conflicts here are
// benign: the informer cache served a stale object — typically to the
// watch-triggered duplicate of the reconcile whose own write just bumped the
// resourceVersion (observed 2026-07-15: the duplicate re-ran provisioning
// end-to-end, then errored "the object has been modified"). Surfacing the
// error logs a Reconciler error and retries with backoff against the same
// stale view; requeueing re-reads fresh state and no-ops.
func requeueOnConflict(err error) (ctrl.Result, error) {
	if errors.IsConflict(err) {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, err
}

// cnpgPhaseHealthy is CNPG's terminal "everything applied and running" phase.
const cnpgPhaseHealthy = "Cluster in healthy state"

// cnpgClusterSettled reports whether a CNPG cluster is fully SETTLED — not just
// serving. During a snapshot-recovery bootstrap the cluster reaches
// readyInstances>=1 BEFORE CNPG's final config-apply restart of the promoted
// primary; publishing Ready on readyInstances alone hands CI a DSN whose -rw
// endpoints go dark again seconds later (observed 2026-07-15: TCP fine at
// Ready+2s, Postgres protocol connect timed out at +95s, fine at +157s —
// exactly the restart window). status.phase only reaches "Cluster in healthy
// state" once every pending restart has been applied, so gate on both.
func cnpgClusterSettled(cluster *unstructured.Unstructured) (bool, string) {
	ready, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
	phase, _, _ := unstructured.NestedString(cluster.Object, "status", "phase")
	return ready >= 1 && phase == cnpgPhaseHealthy,
		fmt.Sprintf("readyInstances=%d phase=%q", ready, phase)
}

// endpointsReady reports whether an Endpoints object has at least one ready address.
func endpointsReady(eps *corev1.Endpoints) bool {
	for _, s := range eps.Subsets {
		if len(s.Addresses) > 0 {
			return true
		}
	}
	return false
}
