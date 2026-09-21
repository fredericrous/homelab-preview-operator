package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
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

const (
	// MigrationCheckJobsLabel is the namespace label a MigrationCheck's
	// namespace must carry for the operator to run a probe Job in it. A CR in
	// any other namespace cannot make the operator run a container there.
	MigrationCheckJobsLabel = "preview.homelab.io/migration-check-jobs"

	// MigrationJobLabel carries the owning MigrationCheck's name on its probe
	// Job. Teardown selects on it; there are no ownerReferences so the Job's
	// lifetime is explicit.
	MigrationJobLabel = "preview.homelab.io/migration-check"

	// probePullGrace is how long past the probe's own timeout the reconciler
	// waits before calling time: it covers pulling the app image, which the
	// in-Job poll cannot see.
	probePullGrace = 3 * time.Minute

	// probeJobSlack is how much later than the reconciler's own deadline the
	// Job's activeDeadlineSeconds fires. The reconciler must judge first: when
	// the Job controller hits its deadline it deletes the pod, and with it the
	// log tail that explains the verdict.
	probeJobSlack = 2 * time.Minute
)

// migrationProbeScript polls the app on localhost until it answers 2xx (and,
// when PROBE_EXPECT is set, contains that substring). The CONTAINER decides the
// verdict through its exit code; the log is detail. It is operator-owned so an
// app cannot write its own pass condition into the spec.
const migrationProbeScript = `set -u
i=0
out=""
while [ "$i" -lt "$PROBE_TIMEOUT" ]; do
  out=$(curl -sS -m 5 -w '\nPROBE_HTTP=%{http_code}' "http://127.0.0.1:${PROBE_PORT}${PROBE_PATH}" 2>&1) || true
  code=$(printf '%s\n' "$out" | sed -n 's/^PROBE_HTTP=//p' | tail -n 1)
  case "$code" in
    2*)
      if [ -z "$PROBE_EXPECT" ] || printf '%s' "$out" | grep -qF -- "$PROBE_EXPECT"; then
        printf '%s\n' "$out"
        echo "probe: ready after ${i}s"
        exit 0
      fi
      ;;
  esac
  i=$((i + 2))
  sleep 2
done
printf '%s\n' "$out"
echo "probe: ${PROBE_PATH} not ready after ${PROBE_TIMEOUT}s"
exit 1
`

// migrationProbeJobName derives the probe Job name from the CR UID, never from
// a PR number, so a re-created CR for the same PR cannot collide with a Job
// that is still terminating.
func migrationProbeJobName(id string) string {
	return "migration-probe-" + id
}

// probeTimeout is the app's boot budget.
func probeTimeout(p *previewv1.MigrationProbe) time.Duration {
	if p == nil || p.TimeoutSeconds <= 0 {
		return 600 * time.Second
	}
	return time.Duration(p.TimeoutSeconds) * time.Second
}

// migrationProbeJob builds the probe Job for a Ready MigrationCheck.
//
// The app image runs as a native sidecar (an init container with
// restartPolicy Always) with its own entrypoint, so the spec never carries a
// start command. DATABASE_URL is the ONLY secret-backed variable and it is
// built here from the connection Secret the operator itself published; every
// spec-supplied variable is a plain value. The probe container is the
// operator's curl image running migrationProbeScript against localhost, and
// its exit code is the Job's.
func (r *MigrationCheckReconciler) migrationProbeJob(mc *previewv1.MigrationCheck, id string) *batchv1.Job {
	p := mc.Spec.Probe
	port := p.Port
	if port <= 0 {
		port = 3000
	}
	path := p.ReadyPath
	if path == "" {
		path = "/"
	}
	timeout := probeTimeout(p)
	// The Job's own deadline includes room to pull the image and lands after
	// the reconciler's (timeout + probePullGrace), so the pod is still there to
	// be tailed when the verdict is written.
	deadline := int64((timeout + probePullGrace + probeJobSlack).Seconds())
	backoff := int32(0)
	ttl := checkJobTTL
	automount := false
	sidecar := corev1.ContainerRestartPolicyAlways

	appEnv := []corev1.EnvVar{{
		Name: "DATABASE_URL",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: mc.Status.ConnectionSecretName},
			Key:                  "DATABASE_URL",
		}},
	}}
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable ordering: a map-ordered env re-writes the Job spec on every reconcile
	for _, k := range keys {
		if k == "DATABASE_URL" {
			continue // the CRD rejects this too; belt and braces
		}
		appEnv = append(appEnv, corev1.EnvVar{Name: k, Value: p.Env[k]})
	}

	containerSC := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if p.RunAsUser != nil {
		// A numeric uid lets the kubelet prove non-root for images whose USER is
		// a name (which it otherwise refuses under runAsNonRoot).
		containerSC.RunAsUser = ptr(*p.RunAsUser)
	}
	probeSC := &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		ReadOnlyRootFilesystem:   ptr(true),
		RunAsNonRoot:             ptr(true),
		RunAsUser:                ptr(int64(100)),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}

	labels := map[string]string{
		MigrationJobLabel:              mc.Name,
		"preview.homelab.io/type":      "migration-probe",
		"app.kubernetes.io/managed-by": "homelab-preview-operator",
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      migrationProbeJobName(id),
			Namespace: mc.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: &automount,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					InitContainers: []corev1.Container{{
						Name:            "app",
						Image:           p.Image,
						ImagePullPolicy: corev1.PullAlways,
						RestartPolicy:   &sidecar,
						Env:             appEnv,
						SecurityContext: containerSC,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("2Gi"),
							},
						},
					}},
					Containers: []corev1.Container{{
						Name:    "probe",
						Image:   r.ProbeImage,
						Command: []string{"/bin/sh", "-c", migrationProbeScript},
						Env: []corev1.EnvVar{
							{Name: "PROBE_PORT", Value: strconv.Itoa(int(port))},
							{Name: "PROBE_PATH", Value: path},
							{Name: "PROBE_EXPECT", Value: p.Expect},
							{Name: "PROBE_TIMEOUT", Value: strconv.FormatInt(int64(timeout.Seconds()), 10)},
						},
						SecurityContext: probeSC,
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

// probeWait is a state to publish instead of a Job: the namespace refused the
// Job for a reason about the cluster, not about the change.
type probeWait struct {
	message string
	requeue time.Duration
}

// ensureProbeJob gets-or-creates the probe Job.
//
// Structural refusal first: the CR's namespace must opt in with
// MigrationCheckJobsLabel=true. A non-nil error is the operator's own problem
// (RBAC, API), surfaced as a reconcile error so it retries with backoff instead
// of being written down as a verdict about somebody's pull request.
func (r *MigrationCheckReconciler) ensureProbeJob(ctx context.Context, desired *batchv1.Job) (*batchv1.Job, *probeWait, error) {
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: desired.Namespace}, ns); err != nil {
		return nil, nil, fmt.Errorf("read namespace %s: %w", desired.Namespace, err)
	}
	if ns.Labels[MigrationCheckJobsLabel] != "true" {
		return nil, nil, fmt.Errorf("refusing to create job %s/%s: namespace is not labelled %s=true",
			desired.Namespace, desired.Name, MigrationCheckJobsLabel)
	}

	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	switch {
	case err == nil && !existing.DeletionTimestamp.IsZero():
		return nil, &probeWait{message: "waiting for the previous probe job to finish terminating", requeue: 5 * time.Second}, nil
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
			return got, nil, nil
		case classifyForbidden(err) == forbiddenQuota:
			return nil, &probeWait{message: fmt.Sprintf("quota refused the probe job: %v", err), requeue: 15 * time.Second}, nil
		}
		return nil, nil, fmt.Errorf("create job %s/%s: %w", desired.Namespace, desired.Name, err)
	}
	return desired, nil, nil
}

// probeWaitingFailure reports why the probe Job's pod is stuck before its
// containers run, if it is: an image that cannot be pulled (reason
// ImageUnavailable) or a container the kubelet refuses to create
// (PodNotStarted — e.g. runAsNonRoot with a non-numeric image user). Both are
// misses about the artifact or the configuration, never a verdict about the
// migrations, so the caller publishes them as Expired.
func (r *MigrationCheckReconciler) probeWaitingFailure(ctx context.Context, namespace, jobName string) (reason, detail string) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{"job-name": jobName}); err != nil {
		return "", ""
	}
	for i := range pods.Items {
		statuses := append([]corev1.ContainerStatus{}, pods.Items[i].Status.InitContainerStatuses...)
		statuses = append(statuses, pods.Items[i].Status.ContainerStatuses...)
		for _, cs := range statuses {
			if cs.State.Waiting == nil {
				continue
			}
			w := cs.State.Waiting
			switch w.Reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "ErrImageNeverPull":
				return previewv1.MigrationReasonImageUnavailable, fmt.Sprintf("%s: %s", w.Reason, w.Message)
			case "CreateContainerConfigError", "CreateContainerError":
				return previewv1.MigrationReasonPodNotStarted, fmt.Sprintf("%s (%s): %s", w.Reason, cs.Name, w.Message)
			}
		}
	}
	return "", ""
}

// deleteProbeJob removes the probe Job with background propagation. Jobs
// default to orphaning their pods; a left-behind app pod would keep hammering
// a database that is about to disappear.
func (r *MigrationCheckReconciler) deleteProbeJob(ctx context.Context, mc *previewv1.MigrationCheck) error {
	name := mc.Status.ProbeJobName
	if name == "" {
		name = migrationProbeJobName(migCheckID(mc))
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mc.Namespace}}
	err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if err != nil && !apierrors.IsNotFound(err) && !namespaceUnavailable(err) {
		return fmt.Errorf("delete probe job %s/%s: %w", mc.Namespace, name, err)
	}
	return nil
}
