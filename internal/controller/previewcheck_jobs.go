package controller

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	previewv1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

// checkJobTTL is how long a finished check Job sticks around. Long enough for a
// human to read the pod logs after a failed verdict, short enough that the
// preview's 20-pod quota is not held by yesterday's probes.
const checkJobTTL int32 = 900

// probeScript is the http check. The CONTAINER decides the verdict: it exits 0
// when the status is expected, 2 when the app answered something else, and 1
// when it could not get an answer at all. Job success is therefore the verdict
// and the logs are only detail — which is what lets the check work without a
// Pod informer, and what keeps a truncated or missing log tail from turning a
// clean run into a failure.
const probeScript = `set -eu
code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time "${PROBE_TIMEOUT}" "${TARGET_URL}") || {
  echo "probe: request to ${TARGET_URL} failed"
  exit 1
}
echo "probe: status=${code}"
for want in ${EXPECT_STATUS}; do
  if [ "${code}" = "${want}" ]; then
    echo "probe: accepted"
    exit 0
  fi
done
echo "probe: unexpected status ${code}, expected one of ${EXPECT_STATUS}"
exit 2
`

// jobName is the check Job's name: derived from the CR UID, never the PR
// number, so a reopened or force-pushed PR cannot collide with the previous
// run's Jobs while those are still terminating.
func jobName(id string, check previewv1.PreviewCheckName) string {
	return fmt.Sprintf("preview-check-%s-%s", id, check)
}

// listCheckJobs returns every check Job belonging to one PreviewCheck.
func (r *PreviewCheckReconciler) listCheckJobs(ctx context.Context, namespace, crName string) ([]batchv1.Job, error) {
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(namespace), client.MatchingLabels{CheckJobLabel: crName}); err != nil {
		return nil, err
	}
	return jobs.Items, nil
}

// forbiddenKind separates the three very different things a 403 on a Job
// create means.
type forbiddenKind int

const (
	// forbiddenOther is an RBAC gap: OUR problem, never the change's.
	forbiddenOther forbiddenKind = iota
	// forbiddenQuota is the preview's ResourceQuota being full.
	forbiddenQuota
	// forbiddenTerminating is the preview namespace going away underneath us.
	forbiddenTerminating
)

// classifyForbidden tells the three apart.
//
// They arrive as the same `Forbidden` status and have historically been handled
// identically, which is wrong in every direction: a full quota deserves a retry
// to the deadline and then an INCONCLUSIVE verdict, a terminating namespace
// deserves an immediate terminal one with no retry at all, and an RBAC gap
// deserves to be a loud reconcile error rather than a quiet verdict about
// somebody's pull request.
func classifyForbidden(err error) forbiddenKind {
	if !apierrors.IsForbidden(err) {
		return forbiddenOther
	}
	var status apierrors.APIStatus
	if stderrors.As(err, &status) {
		if details := status.Status().Details; details != nil {
			for _, cause := range details.Causes {
				if cause.Type == corev1.NamespaceTerminatingCause {
					return forbiddenTerminating
				}
			}
		}
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "exceeded quota"):
		return forbiddenQuota
	case strings.Contains(msg, "being terminated"), strings.Contains(msg, "NamespaceTerminating"):
		return forbiddenTerminating
	}
	return forbiddenOther
}

// namespaceUnavailable reports whether an error means "the namespace is gone or
// going", which on teardown is success rather than failure.
func namespaceUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsNotFound(err) {
		return true
	}
	return classifyForbidden(err) == forbiddenTerminating
}

// ensureJob gets-or-creates a check Job.
//
// The second return value, when non-nil, is a check state to publish instead of
// a Job: the namespace refused us for a reason that is about the preview rather
// than about our code. A non-nil error is the third case — our own problem,
// surfaced as a reconcile error so it retries with backoff and shows up in the
// controller's error metrics instead of being written down as a verdict.
func (r *PreviewCheckReconciler) ensureJob(ctx context.Context, env *checkEnv, desired *batchv1.Job) (*batchv1.Job, *checkStep, error) {
	// Structural refusal: never run anything in a namespace that is not a
	// preview. The reconciler already checked this when it resolved the
	// namespace; repeating it here means no future caller of ensureJob can skip
	// it by accident.
	if env.ns == nil || env.ns.Labels[PreviewEnvironmentLabel] != "true" {
		return nil, nil, fmt.Errorf("refusing to create job %s/%s: namespace is not labelled %s=true",
			desired.Namespace, desired.Name, PreviewEnvironmentLabel)
	}

	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	switch {
	case err == nil && !existing.DeletionTimestamp.IsZero():
		// The previous Job is still terminating. Creating now would earn an
		// AlreadyExists; wait it out.
		return nil, &checkStep{
			phase:   previewv1.CheckRunning,
			message: "waiting for the previous check job to finish terminating",
			requeue: 5 * time.Second,
		}, nil
	case err == nil:
		return existing, nil, nil
	case !apierrors.IsNotFound(err):
		return nil, nil, fmt.Errorf("get job %s/%s: %w", desired.Namespace, desired.Name, err)
	}

	if err := r.Create(ctx, desired); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			got := &batchv1.Job{}
			if gerr := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, got); gerr != nil {
				return nil, nil, fmt.Errorf("re-get job after AlreadyExists: %w", gerr)
			}
			if !got.DeletionTimestamp.IsZero() {
				return nil, &checkStep{
					phase:   previewv1.CheckRunning,
					message: "waiting for the previous check job to finish terminating",
					requeue: 5 * time.Second,
				}, nil
			}
			return got, nil, nil

		case classifyForbidden(err) == forbiddenQuota:
			return nil, &checkStep{
				phase:   previewv1.CheckRunning,
				reason:  previewv1.ReasonQuotaExceeded,
				message: fmt.Sprintf("preview quota refused the check job: %v", err),
				requeue: 15 * time.Second,
			}, nil

		case classifyForbidden(err) == forbiddenTerminating:
			return nil, &checkStep{
				phase:   previewv1.CheckFailed,
				reason:  previewv1.ReasonNamespaceGone,
				crPhase: previewv1.PreviewCheckFailed,
				message: fmt.Sprintf("preview namespace %s is terminating", desired.Namespace),
			}, nil
		}
		return nil, nil, fmt.Errorf("create job %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	return desired, nil, nil
}

func checkJobMeta(env *checkEnv, pcName string, check previewv1.PreviewCheckName) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      jobName(env.id, check),
		Namespace: env.nsName,
		Labels: map[string]string{
			CheckJobLabel:                  pcName,
			CheckNameLabel:                 string(check),
			"preview.homelab.io/type":      "preview-check",
			"app.kubernetes.io/managed-by": "homelab-preview-operator",
		},
		// No ownerReferences, deliberately: the CR lives in another namespace, a
		// cross-namespace ownerRef is invalid, and the garbage collector reaps
		// an object whose owner it cannot find. The finalizer cleans these up.
	}
}

// probeJob builds the http check Job.
//
// It runs IN THE MESH — no `ambient.istio.io/redirection: disabled` annotation,
// unlike the rewrite Job elsewhere in this operator. Preview namespaces carry
// both a ResourceSet-rendered STRICT PeerAuthentication and a Kyverno-generated
// PERMISSIVE one; whichever wins, an in-mesh probe reaches the app, while an
// out-of-mesh one would be refused by the STRICT case and the check would
// report an app failure that is really a mesh failure.
func (r *PreviewCheckReconciler) probeJob(env *checkEnv, pc *previewv1.PreviewCheck, spec previewv1.PreviewCheckSpec, targetURL string) *batchv1.Job {
	statuses := make([]string, 0, len(spec.ExpectStatus))
	for _, s := range spec.ExpectStatus {
		statuses = append(statuses, strconv.Itoa(int(s)))
	}
	// A probe that outlived the run deadline would be pointless; cap it well
	// under it so the Job reports DeadlineExceeded before the check does.
	deadline := int64(60)
	backoff := int32(0)
	ttl := checkJobTTL
	automount := false

	return &batchv1.Job{
		ObjectMeta: checkJobMeta(env, pc.Name, previewv1.CheckHTTP),
		Spec: batchv1.JobSpec{
			// No retries: the container's exit code IS the verdict, so a
			// "failed" pod is a result to read, not a flake to paper over.
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						CheckJobLabel:  pc.Name,
						CheckNameLabel: string(previewv1.CheckHTTP),
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &automount,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr(true),
						RunAsUser:      ptr(int64(100)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:    "probe",
						Image:   r.ProbeImage,
						Command: []string{"/bin/sh", "-c", probeScript},
						Env: []corev1.EnvVar{
							{Name: "TARGET_URL", Value: targetURL},
							{Name: "EXPECT_STATUS", Value: strings.Join(statuses, " ")},
							{Name: "PROBE_TIMEOUT", Value: "20"},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false),
							ReadOnlyRootFilesystem:   ptr(true),
							RunAsNonRoot:             ptr(true),
							RunAsUser:                ptr(int64(100)),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						// Explicit rather than inherited from the preview
						// LimitRange: the probe must fit in whatever the preview
						// has left, and a 100m/128Mi default request against a
						// nearly-full quota is the difference between a verdict
						// and a QuotaExceeded.
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
					}},
				},
			},
		},
	}
}

// smokeJob builds the app's own smoke test Job.
//
// The security context is weaker than the probe's on purpose: this runs a
// user-supplied image, so runAsUser and a read-only root filesystem would break
// most real test images. What stays non-negotiable is the part that protects
// the CLUSTER rather than the container — no service account token (the preview
// namespace holds a reflected repo-write git token and LLM credentials), no
// privilege escalation, all capabilities dropped, RuntimeDefault seccomp.
//
// PREVIEW_HOST_URL is deliberately NOT injected: the edge listener for
// *.preview.daddyshome.fr demands a client certificate, so a test that reached
// for the public host would fail in a way that looks like an app bug.
func (r *PreviewCheckReconciler) smokeJob(env *checkEnv, pc *previewv1.PreviewCheck, cfg *previewv1.SmokeTestConfig, previewURL string) *batchv1.Job {
	timeout := cfg.TimeoutSeconds
	if timeout <= 0 {
		timeout = 300
	}
	deadline := int64(timeout)
	backoff := int32(0)
	ttl := checkJobTTL
	automount := false

	vars := []corev1.EnvVar{
		{Name: "PREVIEW_URL", Value: previewURL},
		{Name: "PREVIEW_NAMESPACE", Value: env.nsName},
		{Name: "APP_NAME", Value: env.app},
		{Name: "PR_NUMBER", Value: strconv.Itoa(int(env.prNumber))},
	}
	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable ordering: a map-ordered env re-writes the Job spec on every reconcile
	for _, k := range keys {
		vars = append(vars, corev1.EnvVar{Name: k, Value: cfg.Env[k]})
	}

	return &batchv1.Job{
		ObjectMeta: checkJobMeta(env, pc.Name, previewv1.CheckSmoke),
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						CheckJobLabel:  pc.Name,
						CheckNameLabel: string(previewv1.CheckSmoke),
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &automount,
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:    "smoke",
						Image:   cfg.Image,
						Command: cfg.Command,
						Args:    cfg.Args,
						Env:     vars,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
					}},
				},
			},
		},
	}
}

func ptr[T any](v T) *T { return &v }
