package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	previewv1 "github.com/fredericrous/homelab-preview-operator/api/v1"
	"github.com/fredericrous/homelab-preview-operator/internal/githubapp"
)

// stubReporter records every check run the reconciler would have published.
type stubReporter struct {
	calls []stubReport
	err   error
	id    int64
}

type stubReport struct {
	report     previewv1.MigrationReport
	existingID int64
	run        githubapp.CheckRun
}

func (s *stubReporter) Report(_ context.Context, report previewv1.MigrationReport, existingID int64, run githubapp.CheckRun) (int64, error) {
	s.calls = append(s.calls, stubReport{report: report, existingID: existingID, run: run})
	if s.err != nil {
		return 0, s.err
	}
	if existingID != 0 {
		return existingID, nil
	}
	s.id++
	return s.id, nil
}

func (s *stubReporter) last() githubapp.CheckRun {
	if len(s.calls) == 0 {
		return githubapp.CheckRun{}
	}
	return s.calls[len(s.calls)-1].run
}

const (
	mcName  = "duro-pr-7-abc1234"
	mcNS    = "migration-check"
	mcImage = "ghcr.io/fredericrous/duro-app:pr-abc1234def5678"
)

type mcFixture struct {
	t        *testing.T
	r        *MigrationCheckReconciler
	cl       client.WithWatch
	clock    *clocktesting.FakePassiveClock
	tail     *stubTailer
	reporter *stubReporter
	deletes  *[]deleteCall
}

func newMCFixture(t *testing.T, objects ...client.Object) *mcFixture {
	t.Helper()
	deletes := &[]deleteCall{}
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&previewv1.MigrationCheck{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				o := &client.DeleteOptions{}
				o.ApplyOptions(opts)
				*deletes = append(*deletes, deleteCall{name: obj.GetName(), options: o})
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	fakeClock := clocktesting.NewFakePassiveClock(time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))
	tail := &stubTailer{out: "[migrations] 37_approval_policy_unique_scope applied\nprobe: ready after 14s"}
	rep := &stubReporter{}
	return &mcFixture{
		t: t, cl: cl, clock: fakeClock, tail: tail, reporter: rep, deletes: deletes,
		r: &MigrationCheckReconciler{
			Client:     cl,
			Log:        logf.Log.WithName("test"),
			Scheme:     testScheme(t),
			Clock:      fakeClock,
			Logs:       tail,
			ProbeImage: "curlimages/curl:8.11.1",
			Reporter:   rep,
		},
	}
}

func (f *mcFixture) reconcile() ctrl.Result {
	f.t.Helper()
	res, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mcNS, Name: mcName}})
	if err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (f *mcFixture) reconcileExpectingError() error {
	f.t.Helper()
	_, err := f.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mcNS, Name: mcName}})
	if err == nil {
		f.t.Fatal("expected a reconcile error")
	}
	return err
}

func (f *mcFixture) get() *previewv1.MigrationCheck {
	f.t.Helper()
	mc := &previewv1.MigrationCheck{}
	if err := f.cl.Get(context.Background(), types.NamespacedName{Namespace: mcNS, Name: mcName}, mc); err != nil {
		f.t.Fatalf("get migrationcheck: %v", err)
	}
	return mc
}

func (f *mcFixture) jobs() []batchv1.Job {
	f.t.Helper()
	list := &batchv1.JobList{}
	if err := f.cl.List(context.Background(), list, client.InNamespace(mcNS)); err != nil {
		f.t.Fatalf("list jobs: %v", err)
	}
	return list.Items
}

// completeJob stands in for the kubelet: it marks the single probe Job done.
func (f *mcFixture) completeJob(succeed bool, reason string) {
	f.t.Helper()
	jobs := f.jobs()
	if len(jobs) != 1 {
		f.t.Fatalf("expected exactly one job, got %d", len(jobs))
	}
	job := &jobs[0]
	if succeed {
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	} else {
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason, Message: "job failed"}}
	}
	// The fake client knows Job carries a status subresource, so a plain Update
	// would silently drop the status; write it through the subresource client.
	if err := f.cl.Status().Update(context.Background(), job); err != nil {
		f.t.Fatalf("update job status: %v", err)
	}
}

func jobsNamespace(labelled bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: mcNS, Labels: map[string]string{}}}
	if labelled {
		ns.Labels[MigrationCheckJobsLabel] = "true"
	}
	return ns
}

func readyCheck(mutate ...func(*previewv1.MigrationCheck)) *previewv1.MigrationCheck {
	created := time.Date(2026, 9, 21, 11, 50, 0, 0, time.UTC)
	mc := &previewv1.MigrationCheck{
		ObjectMeta: metav1.ObjectMeta{
			Name: mcName, Namespace: mcNS, UID: "0e8c0a3b-1c2d-4e5f-8a9b-0c1d2e3f4a5b",
			CreationTimestamp: metav1.Time{Time: created},
			Finalizers:        []string{migrationCheckFinalizer},
		},
		Spec: previewv1.MigrationCheckSpec{
			AppName: "duro", TTLSeconds: 3600, ReadyTimeoutSeconds: 1500,
			Probe: &previewv1.MigrationProbe{
				Image: mcImage, Port: 3000, ReadyPath: "/health/ready", Expect: `"status":"ready"`,
				Env: map[string]string{"SMTP_HOST": "localhost", "SESSION_SECRET": "dummy"}, TimeoutSeconds: 600,
				RunAsUser: ptr(int64(1001)),
			},
			Report: &previewv1.MigrationReport{Provider: "github", Repo: "fredericrous/duro-app", Revision: "abc1234def5678abc1234def5678abc1234def56", Name: "Migration check (prod-data clone)"},
		},
		Status: previewv1.MigrationCheckStatus{
			Phase: previewv1.MigrationCheckReady, Reason: previewv1.MigrationReasonReady,
			ConnectionSecretName: mcName + "-db", ConnectionSecretNamespace: mcNS,
			CloneNamespace: "migration-check-duro-0e8c0a3b1c2d",
			CloneImage:     "ghcr.io/cloudnative-pg/postgresql:17",
			StartedAt:      &metav1.Time{Time: created},
			ExpiresAt:      &metav1.Time{Time: created.Add(time.Hour)},
		},
	}
	for _, m := range mutate {
		m(mc)
	}
	return mc
}

func TestMigrationCheck_ReadyWithoutProbeParks(t *testing.T) {
	f := newMCFixture(t, jobsNamespace(true), readyCheck(func(mc *previewv1.MigrationCheck) {
		mc.Spec.Probe = nil
		mc.Spec.Report = nil
	}))
	res := f.reconcile()
	if got := f.get(); got.Status.Phase != previewv1.MigrationCheckReady {
		t.Fatalf("phase = %s, want Ready", got.Status.Phase)
	}
	if len(f.jobs()) != 0 {
		t.Fatal("no probe Job must be created without spec.probe")
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("expected a requeue towards the TTL, got %+v", res)
	}
}

func TestMigrationCheck_StartProbeCreatesJob(t *testing.T) {
	f := newMCFixture(t, jobsNamespace(true), readyCheck())
	f.reconcile()

	got := f.get()
	if got.Status.Phase != previewv1.MigrationCheckRunning || got.Status.Reason != previewv1.MigrationReasonProbeRunning {
		t.Fatalf("phase/reason = %s/%s", got.Status.Phase, got.Status.Reason)
	}
	jobs := f.jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	job := jobs[0]
	if job.Name != got.Status.ProbeJobName || job.Namespace != mcNS {
		t.Fatalf("job %s/%s not recorded as %q", job.Namespace, job.Name, got.Status.ProbeJobName)
	}
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("probe pod must not mount a service account token")
	}
	if len(pod.InitContainers) != 2 || pod.InitContainers[0].Name != "wait-db" || pod.InitContainers[1].Image != mcImage {
		t.Fatalf("init containers must be [wait-db, app]: %+v", pod.InitContainers)
	}
	waitDB := pod.InitContainers[0]
	if waitDB.Image != "ghcr.io/cloudnative-pg/postgresql:17" || waitDB.RestartPolicy != nil {
		t.Fatalf("wait-db must run pg_isready from the clone's image as a regular init container: %+v", waitDB)
	}
	if waitDB.SecurityContext.RunAsUser == nil || *waitDB.SecurityContext.RunAsUser != 26 {
		t.Fatalf("wait-db must run as the postgres uid: %+v", waitDB.SecurityContext)
	}
	if len(waitDB.Env) == 0 || waitDB.Env[0].Name != "DATABASE_URL" || waitDB.Env[0].ValueFrom == nil || waitDB.Env[0].ValueFrom.SecretKeyRef.Name != mcName+"-db" {
		t.Fatalf("wait-db must read DATABASE_URL from the connection Secret: %+v", waitDB.Env)
	}
	app := pod.InitContainers[1]
	if app.RestartPolicy == nil || *app.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Fatal("app container must be a native sidecar (restartPolicy Always)")
	}
	if len(app.Command) != 0 || len(app.Args) != 0 {
		t.Fatalf("without probe.command the image entrypoint must be kept: %v %v", app.Command, app.Args)
	}
	dbRefs := 0
	for _, e := range app.Env {
		if e.Name == "DATABASE_URL" {
			dbRefs++
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil || e.ValueFrom.SecretKeyRef.Name != mcName+"-db" {
				t.Fatalf("DATABASE_URL must come from the connection Secret, got %+v", e)
			}
		}
	}
	if dbRefs != 1 {
		t.Fatalf("DATABASE_URL injected %d times, want 1", dbRefs)
	}
	// spec env is sorted and plain
	names := []string{}
	for _, e := range app.Env[1:] {
		names = append(names, e.Name)
		if e.ValueFrom != nil {
			t.Fatalf("spec env %s must be a plain value", e.Name)
		}
	}
	if strings.Join(names, ",") != "SESSION_SECRET,SMTP_HOST" {
		t.Fatalf("env order = %v", names)
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Image != "curlimages/curl:8.11.1" {
		t.Fatalf("probe container = %+v", pod.Containers)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != int64((probeBudget(got.Spec.Probe)+probeJobSlack).Seconds()) {
		t.Fatalf("activeDeadlineSeconds = %v", job.Spec.ActiveDeadlineSeconds)
	}
	if got := *job.Spec.ActiveDeadlineSeconds; got <= int64(probeBudget(f.get().Spec.Probe).Seconds()) {
		t.Fatalf("the Job deadline (%d) must land after the reconciler's, or the pod is gone before the log tail", got)
	}
	if app.SecurityContext.RunAsUser == nil || *app.SecurityContext.RunAsUser != 1001 {
		t.Fatalf("runAsUser not propagated to the app container: %+v", app.SecurityContext)
	}
	if run := f.reporter.last(); run.Status != "in_progress" || run.HeadSHA != got.Spec.Report.Revision || run.Name != "Migration check (prod-data clone)" {
		t.Fatalf("expected an in_progress check run on the head sha, got %+v", run)
	}
	if got.Status.CheckRunID == 0 || got.Status.ReportedPhase != previewv1.MigrationCheckRunning {
		t.Fatalf("check run not recorded: id=%d reported=%s", got.Status.CheckRunID, got.Status.ReportedPhase)
	}
}

func TestMigrationCheck_ProbeCommandOverridesEntrypoint(t *testing.T) {
	f := newMCFixture(t, jobsNamespace(true), readyCheck(func(mc *previewv1.MigrationCheck) {
		mc.Spec.Probe.Command = []string{"/api"}
		mc.Spec.Probe.Args = []string{"-port", "8080"}
	}))
	f.reconcile()
	jobs := f.jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	app := jobs[0].Spec.Template.Spec.InitContainers[1]
	if strings.Join(app.Command, " ") != "/api" || strings.Join(app.Args, " ") != "-port 8080" {
		t.Fatalf("command/args not applied: %v %v", app.Command, app.Args)
	}
}

func TestMigrationCheck_UnlabelledNamespaceRefused(t *testing.T) {
	f := newMCFixture(t, jobsNamespace(false), readyCheck())
	err := f.reconcileExpectingError()
	if !strings.Contains(err.Error(), MigrationCheckJobsLabel) {
		t.Fatalf("error should name the opt-in label, got %v", err)
	}
	if len(f.jobs()) != 0 {
		t.Fatal("no Job may be created in an unlabelled namespace")
	}
}

func TestMigrationCheck_ProbeSucceedsPasses(t *testing.T) {
	f := newMCFixture(t, jobsNamespace(true), readyCheck())
	f.reconcile()
	f.completeJob(true, "")
	res := f.reconcile()

	got := f.get()
	if got.Status.Phase != previewv1.MigrationCheckPassed || got.Status.Reason != previewv1.MigrationReasonProbePassed {
		t.Fatalf("phase/reason = %s/%s: %s", got.Status.Phase, got.Status.Reason, got.Status.Message)
	}
	if !strings.Contains(got.Status.Message, "37_approval_policy_unique_scope applied") {
		t.Fatalf("log tail missing from message: %q", got.Status.Message)
	}
	if got.Status.CompletedAt == nil {
		t.Fatal("completedAt not set")
	}
	if f.tail.calls != 1 {
		t.Fatalf("tailer calls = %d, want 1", f.tail.calls)
	}
	run := f.reporter.last()
	if run.Status != "completed" || run.Conclusion != "success" || !strings.Contains(run.Text, "37_approval_policy_unique_scope") {
		t.Fatalf("check run = %+v", run)
	}
	if f.reporter.calls[len(f.reporter.calls)-1].existingID != 1 {
		t.Fatal("the terminal report must PATCH the check run created at start, not create a new one")
	}
	if res.RequeueAfter != 0 || res.Requeue {
		t.Fatalf("terminal phase must not requeue, got %+v", res)
	}
}

func TestMigrationCheck_ProbeFailsFails(t *testing.T) {
	f := newMCFixture(t, jobsNamespace(true), readyCheck())
	f.reconcile()
	f.tail.out = "Error: relation \"grants\" does not exist\nprobe: /health/ready not ready after 600s"
	f.completeJob(false, "BackoffLimitExceeded")
	f.reconcile()

	got := f.get()
	if got.Status.Phase != previewv1.MigrationCheckFailed || got.Status.Reason != previewv1.MigrationReasonProbeFailed {
		t.Fatalf("phase/reason = %s/%s", got.Status.Phase, got.Status.Reason)
	}
	if !strings.Contains(got.Status.Message, `relation "grants" does not exist`) {
		t.Fatalf("log tail missing: %q", got.Status.Message)
	}
	if run := f.reporter.last(); run.Conclusion != "failure" {
		t.Fatalf("conclusion = %q, want failure", run.Conclusion)
	}
}

func TestMigrationCheck_ProbeDeadlineExpires(t *testing.T) {
	f := newMCFixture(t, jobsNamespace(true), readyCheck())
	f.reconcile()
	f.clock.SetTime(f.clock.Now().Add(probeBudget(f.get().Spec.Probe) + time.Minute))
	f.reconcile()

	got := f.get()
	if got.Status.Phase != previewv1.MigrationCheckExpired || got.Status.Reason != previewv1.MigrationReasonDeadlineExceeded {
		t.Fatalf("phase/reason = %s/%s", got.Status.Phase, got.Status.Reason)
	}
	if run := f.reporter.last(); run.Conclusion != "timed_out" {
		t.Fatalf("conclusion = %q, want timed_out", run.Conclusion)
	}
}

func TestMigrationCheck_ImagePullBackOffExpiresNotFails(t *testing.T) {
	f := newMCFixture(t, jobsNamespace(true), readyCheck())
	f.reconcile()
	jobName := f.get().Status.ProbeJobName
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: jobName + "-x1", Namespace: mcNS, Labels: map[string]string{"job-name": jobName}},
		Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  "app",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "manifest unknown"}},
		}}},
	}
	if err := f.cl.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}

	// Before the deadline: still running, message says why.
	f.reconcile()
	if got := f.get(); got.Status.Phase != previewv1.MigrationCheckRunning || !strings.Contains(got.Status.Message, "ImagePullBackOff") {
		t.Fatalf("phase=%s message=%q", got.Status.Phase, got.Status.Message)
	}

	f.clock.SetTime(f.clock.Now().Add(probeBudget(f.get().Spec.Probe) + time.Minute))
	f.reconcile()
	got := f.get()
	if got.Status.Phase != previewv1.MigrationCheckExpired || got.Status.Reason != previewv1.MigrationReasonImageUnavailable {
		t.Fatalf("phase/reason = %s/%s, want Expired/ImageUnavailable", got.Status.Phase, got.Status.Reason)
	}
	if run := f.reporter.last(); run.Conclusion != "timed_out" {
		t.Fatalf("conclusion = %q", run.Conclusion)
	}
}

func TestMigrationCheck_CreateContainerConfigErrorExpiresNotFails(t *testing.T) {
	f := newMCFixture(t, jobsNamespace(true), readyCheck())
	f.reconcile()
	jobName := f.get().Status.ProbeJobName
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: jobName + "-x2", Namespace: mcNS, Labels: map[string]string{"job-name": jobName}},
		Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
			Name: "app",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CreateContainerConfigError", Message: "container has runAsNonRoot and image has non-numeric user (appuser)"}},
		}}},
	}
	if err := f.cl.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	f.clock.SetTime(f.clock.Now().Add(probeBudget(f.get().Spec.Probe) + time.Minute))
	f.reconcile()
	got := f.get()
	if got.Status.Phase != previewv1.MigrationCheckExpired || got.Status.Reason != previewv1.MigrationReasonPodNotStarted {
		t.Fatalf("phase/reason = %s/%s, want Expired/PodNotStarted", got.Status.Phase, got.Status.Reason)
	}
	if !strings.Contains(got.Status.Message, "non-numeric user") {
		t.Fatalf("message should carry the kubelet's reason: %q", got.Status.Message)
	}
	if run := f.reporter.last(); run.Conclusion != "timed_out" {
		t.Fatalf("conclusion = %q", run.Conclusion)
	}
}

func TestMigrationCheck_CloneNotReadyExpires(t *testing.T) {
	mc := readyCheck(func(mc *previewv1.MigrationCheck) {
		mc.Status.Phase = previewv1.MigrationCheckProvisioning
		mc.Status.Reason = previewv1.MigrationReasonProvisioning
		mc.Status.ConnectionSecretName = ""
	})
	f := newMCFixture(t, jobsNamespace(true), mc)
	// StartedAt is 10 min before the fake clock; readyTimeout is 25 min: not yet.
	f.clock.SetTime(mc.Status.StartedAt.Add(30 * time.Minute))
	f.reconcile()

	got := f.get()
	if got.Status.Phase != previewv1.MigrationCheckExpired || got.Status.Reason != previewv1.MigrationReasonCloneNotReady {
		t.Fatalf("phase/reason = %s/%s: %s", got.Status.Phase, got.Status.Reason, got.Status.Message)
	}
	if run := f.reporter.last(); run.Conclusion != "timed_out" {
		t.Fatalf("conclusion = %q", run.Conclusion)
	}
}

func TestMigrationCheck_TTLTooShortExpires(t *testing.T) {
	mc := readyCheck(func(mc *previewv1.MigrationCheck) {
		mc.Spec.TTLSeconds = 900 // created 10 min before the clock: 5 min left, probe needs 18
	})
	f := newMCFixture(t, jobsNamespace(true), mc)
	f.reconcile()
	got := f.get()
	if got.Status.Phase != previewv1.MigrationCheckExpired || got.Status.Reason != previewv1.MigrationReasonTTLTooShort {
		t.Fatalf("phase/reason = %s/%s", got.Status.Phase, got.Status.Reason)
	}
	if len(f.jobs()) != 0 {
		t.Fatal("no Job may start without budget")
	}
}

func TestMigrationCheck_DeleteBeforeVerdictCancels(t *testing.T) {
	f := newMCFixture(t, jobsNamespace(true), readyCheck())
	f.reconcile() // Running, check run 1 in progress
	jobName := f.get().Status.ProbeJobName

	if err := f.cl.Delete(context.Background(), f.get()); err != nil {
		t.Fatal(err)
	}
	f.reconcile() // deletion path: cancel + teardown + finalizer removal

	run := f.reporter.last()
	if run.Status != "completed" || run.Conclusion != "cancelled" {
		t.Fatalf("expected a cancelled check run, got %+v", run)
	}
	var jobDelete *deleteCall
	for i := range *f.deletes {
		if (*f.deletes)[i].name == jobName {
			jobDelete = &(*f.deletes)[i]
		}
	}
	if jobDelete == nil {
		t.Fatal("probe Job was not deleted on teardown")
	}
	if jobDelete.options.PropagationPolicy == nil || *jobDelete.options.PropagationPolicy != metav1.DeletePropagationBackground {
		t.Fatalf("probe Job must be deleted with background propagation, got %+v", jobDelete.options.PropagationPolicy)
	}
}

func TestMigrationCheck_ReporterErrorKeepsVerdict(t *testing.T) {
	f := newMCFixture(t, jobsNamespace(true), readyCheck())
	f.reporter.err = errors.New("github: 503")
	f.reconcile()
	f.completeJob(true, "")
	res := f.reconcile()

	got := f.get()
	if got.Status.Phase != previewv1.MigrationCheckPassed {
		t.Fatalf("phase = %s, verdict must not depend on the forge", got.Status.Phase)
	}
	if got.Status.ReportedPhase != "" || got.Status.CheckRunID != 0 {
		t.Fatalf("nothing may be recorded as reported: %s/%d", got.Status.ReportedPhase, got.Status.CheckRunID)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("expected a 30s report retry, got %+v", res)
	}

	// Forge back: the next reconcile reports the terminal phase once and stops.
	f.reporter.err = nil
	res = f.reconcile()
	got = f.get()
	if got.Status.ReportedPhase != previewv1.MigrationCheckPassed || got.Status.CheckRunID == 0 {
		t.Fatalf("report not recorded after recovery: %s/%d", got.Status.ReportedPhase, got.Status.CheckRunID)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("reported terminal phase must not requeue, got %+v", res)
	}
}

func TestCheckRunFor_Mapping(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 30, 0, 0, time.UTC)
	cases := []struct {
		phase                      previewv1.MigrationCheckPhase
		wantStatus, wantConclusion string
	}{
		{previewv1.MigrationCheckPending, "in_progress", ""},
		{previewv1.MigrationCheckProvisioning, "in_progress", ""},
		{previewv1.MigrationCheckRunning, "in_progress", ""},
		{previewv1.MigrationCheckPassed, "completed", "success"},
		{previewv1.MigrationCheckFailed, "completed", "failure"},
		{previewv1.MigrationCheckExpired, "completed", "timed_out"},
	}
	for _, c := range cases {
		mc := readyCheck(func(mc *previewv1.MigrationCheck) {
			mc.Status.Phase = c.phase
			mc.Status.Reason = "SomeReason"
			mc.Status.Message = "first line\nlog line 1\nlog line 2"
		})
		run := checkRunFor(mc, now)
		if run.Status != c.wantStatus || run.Conclusion != c.wantConclusion {
			t.Errorf("%s: status/conclusion = %s/%s, want %s/%s", c.phase, run.Status, run.Conclusion, c.wantStatus, c.wantConclusion)
		}
		if run.Title != "SomeReason" || !strings.HasPrefix(run.Summary, "first line") || !strings.Contains(run.Summary, mcImage) {
			t.Errorf("%s: title=%q summary=%q", c.phase, run.Title, run.Summary)
		}
		if !strings.Contains(run.Text, "log line 2") || !strings.HasPrefix(run.Text, "```") {
			t.Errorf("%s: text=%q", c.phase, run.Text)
		}
		if (run.CompletedAt != nil) != (c.wantStatus == "completed") {
			t.Errorf("%s: completedAt presence wrong", c.phase)
		}
	}
}
