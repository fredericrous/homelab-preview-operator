package controller

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	previewv1 "github.com/fredericrous/homelab-preview-operator/api/v1"
	"github.com/fredericrous/homelab-preview-operator/internal/previewcheck"
)

const migrationCheckFinalizer = "preview.homelab.io/migration-check"

// warmSnapshotMaxAge is how fresh a shared "warm" source snapshot must be to be
// reused by a MigrationCheck. Migration checks validate that migrations apply
// against real data shapes, for which data a few hours old is fine; a generous
// ceiling means most checks in a burst of PR activity restore instantly, while
// the snapshot is still refreshed often enough to stay representative. The first
// check past this window pays the one-off snapshot cost, then it's warm again.
const warmSnapshotMaxAge = 12 * time.Hour

// maxMigrationMessage bounds what status.message (which can carry a log tail)
// puts into etcd.
const maxMigrationMessage = 4096

// MigrationCheckReconciler provisions a throwaway, snapshot-based CNPG clone of a
// production database so an app's migrations can run against real data before
// merge, optionally runs the app against it (spec.probe), publishes the verdict
// (spec.report), and tears everything down on delete / TTL.
//
// Like PreviewCheck it watches nothing but its own CR and Secrets: the probe
// Job is polled by RequeueAfter, never through a Job informer (cmd/main.go
// disables the Job/Pod cache for the whole manager).
type MigrationCheckReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme

	// Clock is injectable so tests can cross a deadline instantly. Nil means
	// the wall clock.
	Clock clock.PassiveClock

	// Logs tails the probe Job for detail. Nil means no detail, never no verdict.
	Logs LogTailer

	// ProbeImage is the curl image the probe container runs.
	ProbeImage string

	// Reporter publishes check runs. Nil disables reporting.
	Reporter CheckRunReporter
}

// +kubebuilder:rbac:groups=preview.homelab.io,resources=migrationchecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=preview.homelab.io,resources=migrationchecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=preview.homelab.io,resources=migrationchecks/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch
// The connection Secret is created on Ready and deleted on teardown.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// The probe Job and its pod (logs, image-pull state).
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// The warm source snapshot is taken as a CNPG cold Backup of a standby (clone.go);
// the Backup is created, watched and replaced when stale — never updated.
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=backups,verbs=get;list;watch;create;delete

// Reconcile drives a MigrationCheck through Pending -> Provisioning -> Ready and,
// with a probe, on through Running to Passed/Failed/Expired; it tears everything
// down on deletion or TTL expiry.
func (r *MigrationCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("migrationcheck", req.NamespacedName)

	mc := &previewv1.MigrationCheck{}
	if err := r.Get(ctx, req.NamespacedName, mc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: run teardown via finalizer.
	if !mc.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(mc, migrationCheckFinalizer) {
			r.reportCancelled(ctx, mc)
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
		if mc.Spec.Probe == nil {
			// Push flow: re-queue near TTL expiry so the clone is GC'd even if
			// CI never deletes it.
			_, remaining := r.ttlExpired(mc)
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
		return r.startProbe(ctx, log, mc)
	case previewv1.MigrationCheckRunning:
		return r.checkProbe(ctx, log, mc)
	default: // Passed, Failed, Expired
		if err := r.publishReport(ctx, mc); err != nil {
			log.V(1).Info("check run report pending", "error", err.Error())
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, nil
	}
}

// provision creates the throwaway namespace and kicks off the snapshot clone.
func (r *MigrationCheckReconciler) provision(ctx context.Context, log logr.Logger, mc *previewv1.MigrationCheck) (ctrl.Result, error) {
	if mc.Status.StartedAt == nil {
		mc.Status.StartedAt = &metav1.Time{Time: r.now()}
	}

	srcCluster, srcNS, _, err := r.resolveSource(ctx, mc)
	if err != nil {
		return r.fail(ctx, log, mc, previewv1.MigrationReasonCloneFailed, err)
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
		return r.fail(ctx, log, mc, previewv1.MigrationReasonCloneFailed, fmt.Errorf("clone from snapshot: %w", err))
	}

	expires := mc.CreationTimestamp.Add(r.ttl(mc))
	mc.Status.Phase = previewv1.MigrationCheckProvisioning
	mc.Status.Reason = previewv1.MigrationReasonProvisioning
	mc.Status.Message = "clone provisioning"
	mc.Status.CloneNamespace = cloneNS
	mc.Status.ExpiresAt = &metav1.Time{Time: expires}
	if err := r.Status().Update(ctx, mc); err != nil {
		return requeueOnConflict(err)
	}
	r.reportBestEffort(ctx, log, mc)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// checkReady waits until the clone actually accepts connections AND has
// settled, then publishes the DATABASE_URL secret. "Ready" means: the CNPG
// cluster is SETTLED (readyInstances>=1 and phase "Cluster in healthy state" —
// see cnpgClusterSettled for why serving alone is not enough) AND the -rw
// Service has a ready endpoint (CNPG only adds the primary to -rw once its
// readiness probe — a real connection check — passes) AND the superuser secret
// exists. Only then does CI get a connection string (#1).
//
// With a probe configured the wait is bounded by spec.readyTimeoutSeconds: a
// clone that never settles is an infrastructure miss, published as Expired so
// the PR is never told its migrations are broken.
func (r *MigrationCheckReconciler) checkReady(ctx context.Context, log logr.Logger, mc *previewv1.MigrationCheck) (ctrl.Result, error) {
	id := migCheckID(mc)
	cloneNS := mc.Status.CloneNamespace
	clusterName := fmt.Sprintf("migcheck-%s", id)

	notReady := func(state string) (ctrl.Result, error) {
		if mc.Spec.Probe != nil && r.readyDeadlinePassed(mc) {
			return r.finish(ctx, log, mc, previewv1.MigrationCheckExpired, previewv1.MigrationReasonCloneNotReady,
				fmt.Sprintf("clone did not settle within %s (%s)", r.readyTimeout(mc), state))
		}
		log.V(1).Info("clone not ready yet", "state", state)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"})
	if err := r.Get(ctx, types.NamespacedName{Namespace: cloneNS, Name: clusterName}, cluster); err != nil {
		if errors.IsNotFound(err) {
			return notReady("cluster not found")
		}
		return ctrl.Result{}, err
	}
	if settled, state := cnpgClusterSettled(cluster); !settled {
		return notReady(state)
	}

	// -rw endpoints ready == primary accepting connections.
	eps := &corev1.Endpoints{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cloneNS, Name: clusterName + "-rw"}, eps); err != nil {
		if errors.IsNotFound(err) {
			return notReady("-rw endpoints not found")
		}
		return ctrl.Result{}, err
	}
	if !endpointsReady(eps) {
		return notReady("-rw endpoints not ready")
	}

	// Superuser secret (CNPG creates <cluster>-superuser with enableSuperuserAccess).
	suSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cloneNS, Name: clusterName + "-superuser"}, suSecret); err != nil {
		if errors.IsNotFound(err) {
			return notReady("superuser secret not found")
		}
		return ctrl.Result{}, err
	}
	user := string(suSecret.Data["username"])
	pass := string(suSecret.Data["password"])
	if user == "" || pass == "" {
		return notReady("superuser secret empty")
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
	mc.Status.Reason = previewv1.MigrationReasonReady
	mc.Status.Message = "clone ready"
	mc.Status.ConnectionSecretName = secretName
	mc.Status.ConnectionSecretNamespace = mc.Namespace
	if img, _, _ := unstructured.NestedString(cluster.Object, "spec", "imageName"); img != "" {
		mc.Status.CloneImage = img
	}
	if err := r.Status().Update(ctx, mc); err != nil {
		return requeueOnConflict(err)
	}
	log.Info("MigrationCheck ready", "secret", secretName, "cloneNamespace", cloneNS)
	if mc.Spec.Probe != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	_, remaining := r.ttlExpired(mc)
	return ctrl.Result{RequeueAfter: remaining}, nil
}

// startProbe creates the probe Job for a Ready clone and moves to Running.
func (r *MigrationCheckReconciler) startProbe(ctx context.Context, log logr.Logger, mc *previewv1.MigrationCheck) (ctrl.Result, error) {
	// The probe needs its whole budget (plus image pull) before the TTL reaps
	// the clone underneath it; a CR created with too little TTL left is an
	// operator-side misconfiguration, not a verdict.
	_, remaining := r.ttlExpired(mc)
	if need := probeBudget(mc.Spec.Probe); remaining < need {
		return r.finish(ctx, log, mc, previewv1.MigrationCheckExpired, previewv1.MigrationReasonTTLTooShort,
			fmt.Sprintf("%s left before TTL, the probe needs %s", remaining.Round(time.Second), need))
	}

	id := migCheckID(mc)
	job, wait, err := r.ensureProbeJob(ctx, r.migrationProbeJob(mc, id))
	if err != nil {
		return ctrl.Result{}, err
	}
	if wait != nil {
		mc.Status.Message = wait.message
		if uerr := r.Status().Update(ctx, mc); uerr != nil {
			return requeueOnConflict(uerr)
		}
		return ctrl.Result{RequeueAfter: wait.requeue}, nil
	}

	mc.Status.Phase = previewv1.MigrationCheckRunning
	mc.Status.Reason = previewv1.MigrationReasonProbeRunning
	mc.Status.Message = fmt.Sprintf("running %s against the clone", mc.Spec.Probe.Image)
	mc.Status.ProbeJobName = job.Name
	if mc.Status.ProbeStartedAt == nil {
		mc.Status.ProbeStartedAt = &metav1.Time{Time: r.now()}
	}
	if err := r.Status().Update(ctx, mc); err != nil {
		return requeueOnConflict(err)
	}
	log.Info("migration probe started", "job", job.Name, "image", mc.Spec.Probe.Image)
	r.reportBestEffort(ctx, log, mc)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// checkProbe polls the probe Job and turns its outcome into the verdict.
func (r *MigrationCheckReconciler) checkProbe(ctx context.Context, log logr.Logger, mc *previewv1.MigrationCheck) (ctrl.Result, error) {
	name := mc.Status.ProbeJobName
	if name == "" {
		name = migrationProbeJobName(migCheckID(mc))
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: mc.Namespace, Name: name}, job); err != nil {
		if errors.IsNotFound(err) {
			return r.finish(ctx, log, mc, previewv1.MigrationCheckExpired, previewv1.MigrationReasonDeadlineExceeded,
				"probe job disappeared before a verdict")
		}
		return ctrl.Result{}, err
	}

	phase, reason, message := previewcheck.JobOutcome(job)
	switch phase {
	case previewcheck.JobSucceeded:
		tail := r.tailProbe(ctx, log, mc.Namespace, name)
		return r.finish(ctx, log, mc, previewv1.MigrationCheckPassed, previewv1.MigrationReasonProbePassed,
			joinDetail(fmt.Sprintf("%s answered %s against the prod-data clone", mc.Spec.Probe.Image, probePath(mc)), tail))
	case previewcheck.JobFailed:
		tail := r.tailProbe(ctx, log, mc.Namespace, name)
		if why, detail := r.probeWaitingFailure(ctx, mc.Namespace, name); why != "" {
			// The pod never got to run the app: an artifact or configuration
			// miss, published as never-judged.
			return r.finish(ctx, log, mc, previewv1.MigrationCheckExpired, why, joinDetail("probe pod never started: "+detail, tail))
		}
		head := fmt.Sprintf("%s did not answer %s against the prod-data clone", mc.Spec.Probe.Image, probePath(mc))
		if reason != "" {
			head += fmt.Sprintf(" (%s: %s)", reason, message)
		}
		return r.finish(ctx, log, mc, previewv1.MigrationCheckFailed, previewv1.MigrationReasonProbeFailed, joinDetail(head, tail))
	}

	// Still running.
	if why, detail := r.probeWaitingFailure(ctx, mc.Namespace, name); why != "" {
		if r.probeDeadlinePassed(mc) {
			return r.finish(ctx, log, mc, previewv1.MigrationCheckExpired, why, "probe pod never started: "+detail)
		}
		if msg := "waiting for the probe pod: " + detail; mc.Status.Message != msg {
			mc.Status.Message = msg
			if err := r.Status().Update(ctx, mc); err != nil {
				return requeueOnConflict(err)
			}
		}
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	if r.probeDeadlinePassed(mc) {
		tail := r.tailProbe(ctx, log, mc.Namespace, name)
		return r.finish(ctx, log, mc, previewv1.MigrationCheckExpired, previewv1.MigrationReasonDeadlineExceeded,
			joinDetail(fmt.Sprintf("probe still running %s after start", r.now().Sub(mc.Status.ProbeStartedAt.Time).Round(time.Second)), tail))
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// finish writes a terminal phase, then publishes it. The verdict is durable
// before anything talks to the forge; a report failure only delays the report.
func (r *MigrationCheckReconciler) finish(ctx context.Context, log logr.Logger, mc *previewv1.MigrationCheck, phase previewv1.MigrationCheckPhase, reason, message string) (ctrl.Result, error) {
	mc.Status.Phase = phase
	mc.Status.Reason = reason
	mc.Status.Message = truncateMigrationMessage(message)
	mc.Status.CompletedAt = &metav1.Time{Time: r.now()}
	if err := r.Status().Update(ctx, mc); err != nil {
		return requeueOnConflict(err)
	}
	log.Info("MigrationCheck finished", "phase", phase, "reason", reason)
	if err := r.publishReport(ctx, mc); err != nil {
		log.V(1).Info("check run report pending", "error", err.Error())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// reportBestEffort publishes a non-terminal phase (the "in progress" spinner)
// without letting a forge hiccup change the reconcile outcome.
func (r *MigrationCheckReconciler) reportBestEffort(ctx context.Context, log logr.Logger, mc *previewv1.MigrationCheck) {
	if err := r.publishReport(ctx, mc); err != nil {
		log.V(1).Info("check run report pending", "error", err.Error())
	}
}

// tailProbe fetches the probe Job's log tail. Best-effort by design: a missing
// log is missing DETAIL, never a different verdict.
func (r *MigrationCheckReconciler) tailProbe(ctx context.Context, log logr.Logger, namespace, name string) string {
	if r.Logs == nil {
		return ""
	}
	tail, err := r.Logs.TailJobLogs(ctx, namespace, name, logTailBytes)
	if err != nil {
		log.V(1).Info("could not tail probe job logs", "job", name, "error", err.Error())
		return ""
	}
	return previewcheck.TruncateLogTail(strings.TrimSpace(tail), logTailBytes)
}

// teardown removes everything this individual clone created. The probe Job goes
// first (background propagation, so its pods stop talking to the database); the
// throwaway namespace deletion reaps the CNPG cluster, PVCs, restore
// VolumeSnapshot and superuser secret; the per-run cluster-scoped
// VolumeSnapshotContent (Retain, so deleting it never touches the underlying
// CSI snapshot) and the result Secret in the CR namespace are deleted
// explicitly (#5). All deletes are idempotent.
//
// The shared "warm" source snapshot (migcheck-warm-<cluster>) is deliberately NOT
// deleted here — it's reused by subsequent checks and only recycled when a later
// check finds it past warmSnapshotMaxAge. The legacy per-run prod snapshot delete
// below is a no-op under warm reuse (that name is no longer created) and stays for
// back-compat with any in-flight non-warm clone.
func (r *MigrationCheckReconciler) teardown(ctx context.Context, log logr.Logger, mc *previewv1.MigrationCheck) error {
	id := migCheckID(mc)

	if mc.Spec.Probe != nil || mc.Status.ProbeJobName != "" {
		if err := r.deleteProbeJob(ctx, mc); err != nil {
			return err
		}
	}

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

func (r *MigrationCheckReconciler) now() time.Time {
	if r.Clock == nil {
		return time.Now()
	}
	return r.Clock.Now()
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
	remaining := deadline.Sub(r.now())
	if remaining <= 0 {
		return true, 0
	}
	return false, remaining
}

// readyTimeout is the clone bring-up budget when a probe is configured.
func (r *MigrationCheckReconciler) readyTimeout(mc *previewv1.MigrationCheck) time.Duration {
	secs := mc.Spec.ReadyTimeoutSeconds
	if secs <= 0 {
		secs = 1500
	}
	return time.Duration(secs) * time.Second
}

func (r *MigrationCheckReconciler) readyDeadlinePassed(mc *previewv1.MigrationCheck) bool {
	start := mc.CreationTimestamp.Time
	if mc.Status.StartedAt != nil {
		start = mc.Status.StartedAt.Time
	}
	return r.now().After(start.Add(r.readyTimeout(mc)))
}

// probeBudget is the reconciler's deadline for a probe: image pull, database
// settle, then the app's own boot budget.
func probeBudget(p *previewv1.MigrationProbe) time.Duration {
	return probeTimeout(p) + probePullGrace + dbSettleGrace
}

func (r *MigrationCheckReconciler) probeDeadlinePassed(mc *previewv1.MigrationCheck) bool {
	if mc.Status.ProbeStartedAt == nil {
		return false
	}
	return r.now().After(mc.Status.ProbeStartedAt.Add(probeBudget(mc.Spec.Probe)))
}

func probePath(mc *previewv1.MigrationCheck) string {
	if mc.Spec.Probe == nil || mc.Spec.Probe.ReadyPath == "" {
		return "/"
	}
	return mc.Spec.Probe.ReadyPath
}

func truncateMigrationMessage(s string) string {
	if len(s) <= maxMigrationMessage {
		return s
	}
	return previewcheck.TruncateLogTail(s, maxMigrationMessage)
}

// fail records a provisioning error. Without a probe this is the push flow's
// terminal Failed (its CI loop fails fast on it); with a probe the change was
// never judged, so it is Expired.
func (r *MigrationCheckReconciler) fail(ctx context.Context, log logr.Logger, mc *previewv1.MigrationCheck, reason string, err error) (ctrl.Result, error) {
	log.Error(err, "MigrationCheck failed")
	phase := previewv1.MigrationCheckFailed
	if mc.Spec.Probe != nil {
		phase = previewv1.MigrationCheckExpired
	}
	return r.finish(ctx, log, mc, phase, reason, err.Error())
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
		id = "n" + mc.Name // fallback; UID is always set in-cluster
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
