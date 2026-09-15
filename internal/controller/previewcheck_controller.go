package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"

	previewv1 "github.com/fredericrous/homelab-preview-operator/api/v1"
	"github.com/fredericrous/homelab-preview-operator/internal/enrichment"
	"github.com/fredericrous/homelab-preview-operator/internal/previewcheck"
)

const (
	// previewCheckFinalizer guards the check Jobs living in a namespace the CR
	// does not own.
	previewCheckFinalizer = "preview.homelab.io/preview-check"

	// CheckJobLabel carries the owning PreviewCheck's name on every Job the
	// operator creates. Teardown selects on it; there are no ownerReferences
	// (see teardown for why).
	CheckJobLabel = "preview.homelab.io/preview-check"

	// CheckNameLabel carries which check a Job belongs to.
	CheckNameLabel = "preview.homelab.io/check"

	// PreviewAppLabel is the namespace label naming the previewed app. It — not
	// spec.app — is authoritative.
	PreviewAppLabel = "preview-app"

	// previewNamespacePrefix + the PR number is the preview namespace.
	previewNamespacePrefix = "preview-pr-"

	// previewKustomizationPrefix + the PR number is the Flux Kustomization the
	// ResourceSet renders inside the preview namespace.
	previewKustomizationPrefix = "preview-app-"

	// selfDeleteGrace is how long a terminal CR outlives its TTL before the
	// operator removes it. The caller is expected to delete its own CRs; this is
	// only a backstop against a caller that died mid-flight, and it is
	// deliberately terminal-phase-only so a running check is never reaped.
	selfDeleteGrace = 24 * time.Hour

	// maxCheckMessage bounds what a check's message (which can carry a log tail)
	// puts into etcd.
	maxCheckMessage = 4096

	// logTailBytes is how much of a failed Job's log is kept as detail.
	logTailBytes = 2 << 10
)

// LogTailer reads the tail of a check Job's pod logs.
//
// It is an interface for one blunt reason: the client-go fake clientset returns
// the literal string "fake logs" for every pod, so a test that asserts a real
// log tail reached status.checks[].message cannot use it.
type LogTailer interface {
	// TailJobLogs returns at most maxBytes of the most recent pod's log for the
	// named Job. It is best-effort: an error means "no detail available", never
	// "the check failed".
	TailJobLogs(ctx context.Context, namespace, jobName string, maxBytes int64) (string, error)
}

// PreviewCheckReconciler turns a preview environment into a machine-readable
// verdict: Flux settled, workloads available, the app answers over HTTP, the
// re-scanned image carries nothing exploitable, and the app's own smoke test
// passes.
//
// It watches PreviewChecks and NOTHING else. There is no Owns(&batchv1.Job{}),
// no Job or Pod informer: this operator would otherwise cache every Job and Pod
// in the cluster to watch the handful it creates. Progress comes from
// RequeueAfter instead, which costs one extra Get per poll and saves a
// cluster-wide informer. cmd/main.go enforces the same thing structurally with
// Client.Cache.DisableFor.
type PreviewCheckReconciler struct {
	client.Client

	Log logr.Logger

	// Logs tails failed check Jobs for detail. Never nil in production.
	Logs LogTailer

	// Clock is injectable so tests can cross a 30-minute deadline instantly.
	Clock clock.PassiveClock

	// Recorder publishes an Event at every phase/reason transition.
	Recorder record.EventRecorder

	// PreviewDomain builds the (human-only) preview host.
	PreviewDomain string

	// ProbeImage is the image the http check's curl Job runs.
	ProbeImage string

	// Enricher answers KEV/EPSS. Nil (no --cve-enrichment-url) makes the trivy
	// check fail closed rather than read an unenriched scan as clean.
	Enricher enrichment.CVEEnricher
}

// +kubebuilder:rbac:groups=preview.homelab.io,resources=previewchecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=preview.homelab.io,resources=previewchecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=preview.homelab.io,resources=previewchecks/finalizers,verbs=update
// +kubebuilder:rbac:groups=aquasecurity.github.io,resources=vulnerabilityreports,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one PreviewCheck towards a terminal verdict.
func (r *PreviewCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("previewcheck", req.NamespacedName)

	pc := &previewv1.PreviewCheck{}
	if err := r.Get(ctx, req.NamespacedName, pc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion first: the Jobs live in a namespace this CR does not own, so
	// nothing else reaps them.
	if !pc.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(pc, previewCheckFinalizer) {
			if err := r.teardown(ctx, log, pc); err != nil {
				log.Error(err, "teardown failed, will retry")
				return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
			}
			controllerutil.RemoveFinalizer(pc, previewCheckFinalizer)
			if err := r.Update(ctx, pc); err != nil {
				return requeueOnConflict(err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Finalizer before any side effect, so a CR deleted between "Job created"
	// and "finalizer added" cannot leak a probe pod into the preview quota.
	if !controllerutil.ContainsFinalizer(pc, previewCheckFinalizer) {
		controllerutil.AddFinalizer(pc, previewCheckFinalizer)
		if err := r.Update(ctx, pc); err != nil {
			return requeueOnConflict(err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	spec := pc.Spec.Defaulted()
	now := r.Clock.Now()

	// Terminal phases are never left. The only thing that still happens to a
	// terminal CR is the self-delete backstop, well after its TTL.
	if pc.Status.Phase.IsTerminal() {
		if pc.Status.ExpiresAt != nil && now.After(pc.Status.ExpiresAt.Add(selfDeleteGrace)) {
			log.Info("deleting terminal PreviewCheck past its TTL grace", "phase", pc.Status.Phase)
			return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, pc))
		}
		return ctrl.Result{}, nil
	}

	// First pass: stamp the clock the whole run is measured against.
	if pc.Status.StartedAt == nil {
		pc.Status.StartedAt = &metav1.Time{Time: now}
		pc.Status.ExpiresAt = &metav1.Time{Time: now.Add(time.Duration(spec.TTLSeconds) * time.Second)}
		return r.publish(ctx, log, pc, statusUpdate{
			phase:   previewv1.PreviewCheckPending,
			reason:  previewv1.ReasonPending,
			message: "waiting for the preview environment",
			requeue: 5 * time.Second,
		})
	}

	// TTL: an abandoned check stops costing reconciles.
	if pc.Status.ExpiresAt != nil && !now.Before(pc.Status.ExpiresAt.Time) {
		return r.publish(ctx, log, pc, statusUpdate{
			phase:   previewv1.PreviewCheckExpired,
			reason:  previewv1.ReasonTimeout,
			message: "TTL expired before a verdict was reached",
		})
	}

	elapsed := now.Sub(pc.Status.StartedAt.Time)
	deadline := pc.Status.StartedAt.Add(time.Duration(spec.TimeoutSeconds) * time.Second)
	env := &checkEnv{
		now:          now,
		elapsed:      elapsed,
		pastDeadline: !now.Before(deadline),
		prNumber:     spec.PRNumber,
		nsName:       fmt.Sprintf("%s%d", previewNamespacePrefix, spec.PRNumber),
		id:           previewcheck.CheckID(string(pc.UID), pc.Name),
	}

	// One cached Namespace Get serves three questions: does the preview exist,
	// is it really a preview namespace, and which app does it hold.
	if res, done, err := r.resolveNamespace(ctx, log, pc, spec, env); done || err != nil {
		return res, err
	}

	// The Kustomization gate. Nothing below this line has side effects until
	// Flux says the preview is the thing we were asked to judge.
	if res, done, err := r.checkGate(ctx, log, pc, spec, env); done || err != nil {
		return res, err
	}

	return r.runChecks(ctx, log, pc, spec, env)
}

// checkEnv is the resolved context one reconcile pass runs against.
type checkEnv struct {
	now          time.Time
	elapsed      time.Duration
	pastDeadline bool
	prNumber     int32
	nsName       string
	id           string

	// ns is the preview namespace object, once resolved.
	ns *corev1.Namespace
	// app is the namespace's preview-app label — the authoritative app name.
	app string
}

// resolveNamespace reads the preview namespace once and answers NamespaceGone,
// the preview-environment guard and the app validation from it.
//
// The absence of the namespace is EXPECTED for the first minutes of a check:
// the CR is created when the PR opens, but the ResourceSetInputProvider polls
// every minute and only sees PRs that already carry `preview-ready`, which CI
// adds after the PR exists. So "absent" is Pending, never a verdict — until the
// run deadline, where it becomes Expired (inconclusive: the change was never
// judged). Only a namespace we ONCE OBSERVED and that has since gone is a
// terminal Failed; that one really is "the preview was pulled out from under
// the change".
func (r *PreviewCheckReconciler) resolveNamespace(ctx context.Context, log logr.Logger, pc *previewv1.PreviewCheck, spec previewv1.PreviewCheckSpec, env *checkEnv) (ctrl.Result, bool, error) {
	ns := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: env.nsName}, ns)

	gone := errors.IsNotFound(err) || (err == nil && !ns.DeletionTimestamp.IsZero())
	switch {
	case gone && pc.Status.PreviewNamespace != "":
		res, uerr := r.publish(ctx, log, pc, statusUpdate{
			phase:   previewv1.PreviewCheckFailed,
			reason:  previewv1.ReasonNamespaceGone,
			message: fmt.Sprintf("preview namespace %s was observed and has since been removed", env.nsName),
		})
		return res, true, uerr
	case gone:
		res, uerr := r.pendingOrExpired(ctx, log, pc, env, previewv1.ReasonNamespaceGone,
			fmt.Sprintf("preview namespace %s has not been rendered yet", env.nsName))
		return res, true, uerr
	case err != nil:
		return ctrl.Result{}, true, err
	}

	// A namespace that exists but is not labelled as a preview is either
	// mid-creation or not ours. Either way nothing may run in it.
	if ns.Labels[PreviewEnvironmentLabel] != "true" {
		res, uerr := r.pendingOrExpired(ctx, log, pc, env, previewv1.ReasonNamespaceGone,
			fmt.Sprintf("namespace %s is not labelled %s=true", env.nsName, PreviewEnvironmentLabel))
		return res, true, uerr
	}

	app := ns.Labels[PreviewAppLabel]
	if app == "" {
		res, uerr := r.pendingOrExpired(ctx, log, pc, env, previewv1.ReasonTargetUnresolved,
			fmt.Sprintf("namespace %s carries no %s label yet", env.nsName, PreviewAppLabel))
		return res, true, uerr
	}
	if spec.App != "" && spec.App != app {
		res, uerr := r.publish(ctx, log, pc, statusUpdate{
			phase:  previewv1.PreviewCheckFailed,
			reason: previewv1.ReasonAppMismatch,
			app:    app,
			message: fmt.Sprintf("spec.app %q does not match the preview namespace's %s=%q",
				spec.App, PreviewAppLabel, app),
		})
		return res, true, uerr
	}

	env.ns = ns
	env.app = app
	pc.Status.PreviewNamespace = env.nsName
	pc.Status.PreviewHost = fmt.Sprintf("pr-%d-%s.preview.%s", spec.PRNumber, app, r.PreviewDomain)
	return ctrl.Result{}, false, nil
}

// checkGate holds everything until the preview Kustomization has settled.
//
// "Settled" is deliberately stricter than "Ready": the operator's own
// annotation machine suspends the Kustomization until credentials are patched
// in, and a Ready=True carried over from the generation BEFORE that patch would
// let checks run against a preview still pointing at nothing.
//
// The gate closing mid-check (the operator re-suspends on a PreviewConfig hash
// change) leaves in-flight checks Running rather than failing them — a
// re-render is not a verdict. The deadline decides if it never re-opens.
func (r *PreviewCheckReconciler) checkGate(ctx context.Context, log logr.Logger, pc *previewv1.PreviewCheck, spec previewv1.PreviewCheckSpec, env *checkEnv) (ctrl.Result, bool, error) {
	ks := &kustomizev1.Kustomization{}
	ksName := fmt.Sprintf("%s%d", previewKustomizationPrefix, spec.PRNumber)
	err := r.Get(ctx, types.NamespacedName{Namespace: env.nsName, Name: ksName}, ks)
	if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, true, err
	}

	var settled bool
	var reason, detail string
	var readyFalse bool
	if errors.IsNotFound(err) {
		reason, detail = previewv1.ReasonKustomizationNotReady, fmt.Sprintf("Kustomization %s/%s not found", env.nsName, ksName)
	} else {
		settled, reason, detail, readyFalse = kustomizationSettled(ks, spec.Revision)
	}
	if settled {
		return ctrl.Result{}, false, nil
	}

	if env.pastDeadline {
		// A Kustomization that exists and says Ready=False has judged the
		// change: the manifests in the PR do not apply. That is a negative
		// verdict. Anything vaguer — missing, suspended, still reconciling — is
		// inconclusive, and the caller must not close a PR over it.
		phase := previewv1.PreviewCheckExpired
		if readyFalse || reason == previewv1.ReasonRevisionMismatch {
			phase = previewv1.PreviewCheckFailed
		}
		res, uerr := r.publish(ctx, log, pc, statusUpdate{phase: phase, reason: reason, app: env.app, message: detail})
		return res, true, uerr
	}

	phase := previewv1.PreviewCheckPending
	if anyCheckStarted(pc) {
		phase = previewv1.PreviewCheckRunning
	}
	res, uerr := r.publish(ctx, log, pc, statusUpdate{
		phase:   phase,
		reason:  reason,
		app:     env.app,
		message: detail,
		requeue: requeueLadder(env.elapsed),
	})
	return res, true, uerr
}

// kustomizationSettled reports whether the preview Kustomization has finished
// applying the revision under test.
func kustomizationSettled(ks *kustomizev1.Kustomization, revision string) (settled bool, reason, detail string, readyFalse bool) {
	if ks.Spec.Suspend {
		return false, previewv1.ReasonKustomizationNotReady, "preview Kustomization is still suspended", false
	}
	if ks.Annotations[CredentialsPatchedAnnotation] != "true" {
		return false, previewv1.ReasonKustomizationNotReady, "preview credentials have not been patched in yet", false
	}
	if ks.Generation != ks.Status.ObservedGeneration {
		return false, previewv1.ReasonKustomizationNotReady,
			fmt.Sprintf("preview Kustomization generation %d not observed (at %d)", ks.Generation, ks.Status.ObservedGeneration), false
	}

	ready := apimeta.FindStatusCondition(ks.Status.Conditions, "Ready")
	switch {
	case ready == nil:
		return false, previewv1.ReasonKustomizationNotReady, "preview Kustomization has no Ready condition yet", false
	case ready.ObservedGeneration != 0 && ready.ObservedGeneration != ks.Generation:
		return false, previewv1.ReasonKustomizationNotReady,
			fmt.Sprintf("preview Kustomization Ready condition is stale (observed %d, want %d)", ready.ObservedGeneration, ks.Generation), false
	case ready.Status == metav1.ConditionFalse:
		return false, previewv1.ReasonKustomizationNotReady,
			fmt.Sprintf("preview Kustomization is not Ready: %s: %s", ready.Reason, ready.Message), true
	case ready.Status != metav1.ConditionTrue:
		return false, previewv1.ReasonKustomizationNotReady, "preview Kustomization readiness is unknown", false
	}

	if revision != "" && !strings.HasSuffix(ks.Status.LastAppliedRevision, revision) {
		return false, previewv1.ReasonRevisionMismatch,
			fmt.Sprintf("preview applied %q, which does not end with the requested revision %q", ks.Status.LastAppliedRevision, revision), false
	}
	return true, "", fmt.Sprintf("preview Kustomization applied %s", ks.Status.LastAppliedRevision), false
}

// runChecks advances exactly one check per reconcile, in a fixed order,
// fail-fast on the first failure and never re-running one that has finished.
func (r *PreviewCheckReconciler) runChecks(ctx context.Context, log logr.Logger, pc *previewv1.PreviewCheck, spec previewv1.PreviewCheckSpec, env *checkEnv) (ctrl.Result, error) {
	for _, name := range orderedChecks(spec.Checks) {
		existing := findCheck(pc, name)
		if existing != nil && existing.Phase.IsTerminal() {
			continue
		}
		return r.advanceCheck(ctx, log, pc, spec, env, name, existing)
	}

	return r.publish(ctx, log, pc, statusUpdate{
		phase:   previewv1.PreviewCheckPassed,
		reason:  previewv1.ReasonAllChecksPassed,
		app:     env.app,
		message: fmt.Sprintf("%d check(s) passed", len(pc.Status.Checks)),
	})
}

// advanceCheck runs one step of one check and publishes the result.
func (r *PreviewCheckReconciler) advanceCheck(ctx context.Context, log logr.Logger, pc *previewv1.PreviewCheck, spec previewv1.PreviewCheckSpec, env *checkEnv, name previewv1.PreviewCheckName, existing *previewv1.PreviewCheckResult) (ctrl.Result, error) {
	prevPhase := previewv1.CheckPending
	startedAt := &metav1.Time{Time: env.now}
	if existing != nil {
		prevPhase = existing.Phase
		if existing.StartedAt != nil {
			startedAt = existing.StartedAt
		}
	}

	step, err := r.evaluateCheck(ctx, log, pc, spec, env, name)
	if err != nil {
		// An RBAC gap, an API server error: a reconcile error, never a verdict.
		// Backing off and retrying is right; telling the caller the change is
		// bad because we could not look would not be.
		//
		// But a PERSISTENT error must still honour the run deadline. Returning
		// the error forever leaves the CR Running until the TTL — four times the
		// deadline — and a caller polling for a verdict waits two hours for one
		// that was never coming. Past the deadline the honest answer is the same
		// as for any other thing we could not look at: inconclusive.
		if env.pastDeadline {
			log.Error(err, "check errored past the run deadline, resolving as inconclusive", "check", name)
			return r.publish(ctx, log, pc, statusUpdate{
				phase:   previewv1.PreviewCheckExpired,
				reason:  previewv1.ReasonTimeout,
				app:     env.app,
				message: fmt.Sprintf("%s check could not complete before the run deadline: %v", name, err),
			})
		}
		return ctrl.Result{}, err
	}

	// The run deadline turns an unfinished check into a finished one. Which
	// terminal CR phase that earns is the reason's business, not the deadline's.
	if !step.phase.IsTerminal() && env.pastDeadline {
		step.phase = previewv1.CheckFailed
		switch step.reason {
		case "", previewv1.ReasonRunning:
			step.reason = previewv1.ReasonTimeout
		case previewv1.ReasonScanPending:
			step.reason = previewv1.ReasonScanMissing
		}
		step.message = strings.TrimSpace("run deadline reached: " + step.message)
	}

	res := previewv1.PreviewCheckResult{
		Name:      name,
		Phase:     step.phase,
		Reason:    step.reason,
		Message:   truncateMessage(step.message, maxCheckMessage),
		StartedAt: startedAt,
		Details:   step.details,
	}
	if step.phase.IsTerminal() {
		res.FinishedAt = &metav1.Time{Time: env.now}
	}
	upsertCheck(pc, res)

	u := statusUpdate{app: env.app}
	if !prevPhase.IsTerminal() && step.phase.IsTerminal() {
		u.checkDone = &res
	}

	switch {
	case step.phase == previewv1.CheckFailed:
		u.phase = step.crPhase
		if u.phase == "" {
			u.phase = terminalPhaseFor(step.reason)
		}
		u.reason = step.reason
		u.message = fmt.Sprintf("%s check failed: %s", name, step.message)

	case step.phase.IsTerminal() && allChecksTerminal(pc, spec):
		u.phase = previewv1.PreviewCheckPassed
		u.reason = previewv1.ReasonAllChecksPassed
		u.message = fmt.Sprintf("%d check(s) passed", len(pc.Status.Checks))

	case step.phase.IsTerminal():
		// One check down, more to go: come straight back for the next one.
		u.phase = previewv1.PreviewCheckRunning
		u.reason = previewv1.ReasonRunning
		u.message = fmt.Sprintf("%s check %s", name, strings.ToLower(string(step.phase)))
		u.requeue = time.Second

	default:
		u.phase = previewv1.PreviewCheckRunning
		u.reason = step.reason
		if u.reason == "" {
			u.reason = previewv1.ReasonRunning
		}
		u.message = fmt.Sprintf("%s check running: %s", name, step.message)
		u.requeue = step.requeue
		if u.requeue == 0 {
			u.requeue = requeueLadder(env.elapsed)
		}
	}

	return r.publish(ctx, log, pc, u)
}

// teardown deletes the check Jobs this CR created.
//
// Jobs default to ORPHAN propagation, so a plain Delete leaves the probe pod
// running and burning the preview's ResourceQuota (4 CPU / 8 Gi, 20 pods, for
// the whole preview). Background propagation is therefore not optional — and
// carrying it requires List + per-object Delete: DeleteAllOf cannot express a
// per-object propagation policy and the fake client ignores delete options on
// it anyway, so the unit tests would prove nothing.
//
// Deliberately NO ownerReferences on the Jobs: a cross-namespace ownerRef is
// invalid, and Kubernetes' garbage collector reaps an object whose owner it
// cannot find in its own namespace — the Job would vanish seconds after
// creation.
//
// A missing or terminating preview namespace is success, not an error: the
// preview was pruned, there is nothing to clean up, and a finalizer that
// insisted otherwise would wedge the CR forever.
func (r *PreviewCheckReconciler) teardown(ctx context.Context, log logr.Logger, pc *previewv1.PreviewCheck) error {
	nsName := pc.Status.PreviewNamespace
	if nsName == "" {
		nsName = fmt.Sprintf("%s%d", previewNamespacePrefix, pc.Spec.PRNumber)
	}

	jobs, err := r.listCheckJobs(ctx, nsName, pc.Name)
	if err != nil {
		if namespaceUnavailable(err) {
			log.V(1).Info("preview namespace already gone, nothing to tear down", "namespace", nsName)
			return nil
		}
		return fmt.Errorf("list check jobs in %s: %w", nsName, err)
	}

	for i := range jobs {
		job := &jobs[i]
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
			if errors.IsNotFound(err) || namespaceUnavailable(err) {
				continue
			}
			return fmt.Errorf("delete job %s/%s: %w", job.Namespace, job.Name, err)
		}
	}
	if len(jobs) > 0 {
		log.Info("deleted check jobs", "namespace", nsName, "count", len(jobs))
	}
	return nil
}

// statusUpdate is one published status transition.
type statusUpdate struct {
	phase   previewv1.PreviewCheckPhase
	reason  string
	message string
	requeue time.Duration

	// app is the resolved app name for the metric labels. Empty falls back to
	// spec.app, then to "unknown".
	app string

	// checkDone, when set, is the check that has just reached a terminal phase
	// for the first time. It is counted only once the status write lands.
	checkDone *previewv1.PreviewCheckResult
}

// publish writes the status, emits an Event on a real transition, and counts
// the terminal verdict exactly once.
func (r *PreviewCheckReconciler) publish(ctx context.Context, log logr.Logger, pc *previewv1.PreviewCheck, u statusUpdate) (ctrl.Result, error) {
	prevPhase := pc.Status.Phase
	prevReason := ""
	if c := apimeta.FindStatusCondition(pc.Status.Conditions, previewv1.ConditionSucceeded); c != nil {
		prevReason = c.Reason
	}
	now := r.Clock.Now()

	pc.Status.Phase = u.phase
	pc.Status.Message = truncateMessage(u.message, maxCheckMessage)
	pc.Status.ObservedGeneration = pc.Generation

	condStatus := metav1.ConditionUnknown
	switch u.phase {
	case previewv1.PreviewCheckPassed:
		condStatus = metav1.ConditionTrue
	case previewv1.PreviewCheckFailed, previewv1.PreviewCheckExpired:
		condStatus = metav1.ConditionFalse
	}
	apimeta.SetStatusCondition(&pc.Status.Conditions, metav1.Condition{
		Type:               previewv1.ConditionSucceeded,
		Status:             condStatus,
		Reason:             u.reason,
		Message:            truncateMessage(pc.Status.Message, 32*1024),
		ObservedGeneration: pc.Generation,
		LastTransitionTime: metav1.Time{Time: now},
	})

	becameTerminal := u.phase.IsTerminal() && !prevPhase.IsTerminal()
	if becameTerminal {
		pc.Status.CompletedAt = &metav1.Time{Time: now}
	}

	if err := r.Status().Update(ctx, pc); err != nil {
		return requeueOnConflict(err)
	}

	// Metrics and Events only AFTER the write lands. A conflict returns above
	// without counting, and its retry re-reads a status that is still
	// non-terminal — so the terminal verdict is counted exactly once.
	app := metricApp(pc, u.app)
	if u.checkDone != nil {
		previewCheckCheckTotal.WithLabelValues(app, string(u.checkDone.Name), string(u.checkDone.Phase)).Inc()
	}
	if becameTerminal {
		previewCheckTotal.WithLabelValues(app, string(u.phase), u.reason).Inc()
		if pc.Status.StartedAt != nil {
			previewCheckDuration.WithLabelValues(app).Observe(now.Sub(pc.Status.StartedAt.Time).Seconds())
		}
	}
	if prevPhase != u.phase || prevReason != u.reason {
		r.event(pc, u.phase, u.reason, pc.Status.Message)
		log.Info("PreviewCheck transition", "phase", u.phase, "reason", u.reason, "message", pc.Status.Message)
	}

	if u.phase.IsTerminal() {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: u.requeue}, nil
}

// pendingOrExpired publishes a not-yet state, or resolves it as inconclusive
// once the run deadline has passed.
func (r *PreviewCheckReconciler) pendingOrExpired(ctx context.Context, log logr.Logger, pc *previewv1.PreviewCheck, env *checkEnv, reason, message string) (ctrl.Result, error) {
	if env.pastDeadline {
		return r.publish(ctx, log, pc, statusUpdate{
			phase:   previewv1.PreviewCheckExpired,
			reason:  reason,
			app:     env.app,
			message: message,
		})
	}
	return r.publish(ctx, log, pc, statusUpdate{
		phase:   previewv1.PreviewCheckPending,
		reason:  reason,
		app:     env.app,
		message: message,
		requeue: requeueLadder(env.elapsed),
	})
}

func (r *PreviewCheckReconciler) event(pc *previewv1.PreviewCheck, phase previewv1.PreviewCheckPhase, reason, message string) {
	if r.Recorder == nil {
		return
	}
	kind := corev1.EventTypeNormal
	if phase == previewv1.PreviewCheckFailed || phase == previewv1.PreviewCheckExpired {
		kind = corev1.EventTypeWarning
	}
	if reason == "" {
		reason = previewv1.ReasonRunning
	}
	r.Recorder.Event(pc, kind, reason, truncateMessage(message, 1024))
}

// terminalPhaseFor maps a failure reason to the CR phase that tells the caller
// what to DO about it.
//
// The distinction is the entire point of the Failed/Expired split: Failed means
// the change was judged and judged bad (close the PR); Expired means the
// machinery never managed to judge it (leave the PR open, cool down, escalate).
// Reasons that describe OUR inability to look — no scan, no intelligence, no
// quota, no target to probe, no time — are inconclusive; everything else is a
// verdict. TargetUnresolved belongs on that side: "this namespace has no
// HTTPRoute backend and no non-infrastructure Service I can identify" is a
// statement about the operator's reach, not about the change.
func terminalPhaseFor(reason string) previewv1.PreviewCheckPhase {
	switch reason {
	case previewv1.ReasonScanMissing,
		previewv1.ReasonEnrichmentUnavailable,
		previewv1.ReasonQuotaExceeded,
		previewv1.ReasonTargetUnresolved,
		previewv1.ReasonTimeout:
		return previewv1.PreviewCheckExpired
	default:
		return previewv1.PreviewCheckFailed
	}
}

// orderedChecks returns the selected checks in the fixed execution order,
// regardless of how the spec listed them.
func orderedChecks(selected []previewv1.PreviewCheckName) []previewv1.PreviewCheckName {
	want := map[previewv1.PreviewCheckName]bool{}
	for _, n := range selected {
		want[n] = true
	}
	var out []previewv1.PreviewCheckName
	for _, n := range []previewv1.PreviewCheckName{
		previewv1.CheckReadiness, previewv1.CheckHTTP, previewv1.CheckTrivy, previewv1.CheckSmoke,
	} {
		if want[n] {
			out = append(out, n)
		}
	}
	return out
}

func findCheck(pc *previewv1.PreviewCheck, name previewv1.PreviewCheckName) *previewv1.PreviewCheckResult {
	for i := range pc.Status.Checks {
		if pc.Status.Checks[i].Name == name {
			return &pc.Status.Checks[i]
		}
	}
	return nil
}

func upsertCheck(pc *previewv1.PreviewCheck, res previewv1.PreviewCheckResult) {
	for i := range pc.Status.Checks {
		if pc.Status.Checks[i].Name == res.Name {
			pc.Status.Checks[i] = res
			return
		}
	}
	pc.Status.Checks = append(pc.Status.Checks, res)
}

func allChecksTerminal(pc *previewv1.PreviewCheck, spec previewv1.PreviewCheckSpec) bool {
	for _, name := range orderedChecks(spec.Checks) {
		res := findCheck(pc, name)
		if res == nil || !res.Phase.IsTerminal() {
			return false
		}
	}
	return true
}

func anyCheckStarted(pc *previewv1.PreviewCheck) bool {
	for i := range pc.Status.Checks {
		if pc.Status.Checks[i].Phase != previewv1.CheckPending {
			return true
		}
	}
	return false
}

// requeueLadder backs off as a check ages: tight while a rollout is visibly
// moving, slacker once we are clearly waiting on something with its own clock
// (trivy's scan queue, Flux's five-minute interval).
func requeueLadder(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < time.Minute:
		return 5 * time.Second
	case elapsed < 5*time.Minute:
		return 15 * time.Second
	default:
		return 30 * time.Second
	}
}

func truncateMessage(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// metricApp keeps the cardinality of the metrics bounded and predictable.
//
// The resolved app — the preview namespace's `preview-app` label — is preferred
// over spec.app, because the label is what every other decision in the
// reconciler uses and spec.app is optional. spec.app is the fallback for the
// transitions that happen before the namespace resolves (or that fail because
// the two disagree), and "unknown" the last resort: an empty label value is
// indistinguishable from a broken exporter on a dashboard.
func metricApp(pc *previewv1.PreviewCheck, resolved string) string {
	switch {
	case resolved != "":
		return resolved
	case pc.Spec.App != "":
		return pc.Spec.App
	default:
		return "unknown"
	}
}

// SetupWithManager wires the controller.
//
// MaxConcurrentReconciles is 4, not the default 1: one object is never
// reconciled concurrently anyway, and a single worker would serialise every
// PreviewCheck in the cluster behind whichever one is currently waiting on the
// enrichment endpoint. The rate limiter governs ERRORS only — the RequeueAfter
// ladder bypasses it — so its 5 ms floor is about retrying a conflicted write
// promptly, and its 30 s ceiling about not hammering a broken API server.
func (r *PreviewCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	registerActiveGauge(mgr.GetClient())
	return ctrl.NewControllerManagedBy(mgr).
		For(&previewv1.PreviewCheck{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 4,
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](
				5*time.Millisecond, 30*time.Second),
		}).
		Complete(r)
}
