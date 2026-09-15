package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"

	previewv1 "github.com/fredericrous/homelab-preview-operator/api/v1"
	"github.com/fredericrous/homelab-preview-operator/internal/enrichment"
	"github.com/fredericrous/homelab-preview-operator/internal/previewcheck"
)

// ---------------------------------------------------------------------------
// test doubles
// ---------------------------------------------------------------------------

// stubTailer stands in for PodLogTailer. The client-go fake clientset returns
// the literal string "fake logs" for every pod, so it cannot be used to assert
// that a real log tail reached status.
type stubTailer struct {
	out   string
	err   error
	calls int
}

func (s *stubTailer) TailJobLogs(_ context.Context, _, _ string, _ int64) (string, error) {
	s.calls++
	return s.out, s.err
}

type stubEnricher struct {
	resp  enrichment.Response
	err   error
	calls int
	got   []string
}

func (s *stubEnricher) Lookup(_ context.Context, cves []string) (enrichment.Response, error) {
	s.calls++
	s.got = append([]string(nil), cves...)
	return s.resp, s.err
}

type deleteCall struct {
	name    string
	options *client.DeleteOptions
}

type fixture struct {
	t       *testing.T
	r       *PreviewCheckReconciler
	cl      client.Client
	clock   *clocktesting.FakePassiveClock
	rec     *record.FakeRecorder
	enr     *stubEnricher
	tail    *stubTailer
	deletes *[]deleteCall
}

const (
	testApp   = "navidrome"
	testPR    = 42
	testNS    = "preview-pr-42"
	testImage = "ghcr.io/fredericrous/navidrome:0.55.0"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(kustomizev1.AddToScheme(s))
	utilruntime.Must(previewv1.AddToScheme(s))

	// The operator reads VulnerabilityReports and HTTPRoutes as unstructured
	// (the same way it already reads CNPG Clusters), so the fake client needs
	// the kinds registered even though there is no Go type for them.
	for _, gvk := range []schema.GroupVersionKind{gvkHTTPRoute, gvkVulnReportList.GroupVersion().WithKind("VulnerabilityReport")} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

func newFixture(t *testing.T, objects ...client.Object) *fixture {
	t.Helper()

	deletes := &[]deleteCall{}
	scheme := testScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&previewv1.PreviewCheck{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				o := &client.DeleteOptions{}
				o.ApplyOptions(opts)
				*deletes = append(*deletes, deleteCall{name: obj.GetName(), options: o})
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	fakeClock := clocktesting.NewFakePassiveClock(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	enr := &stubEnricher{resp: cleanEnrichment()}
	tail := &stubTailer{}
	rec := record.NewFakeRecorder(200)

	return &fixture{
		t:       t,
		cl:      cl,
		clock:   fakeClock,
		rec:     rec,
		enr:     enr,
		tail:    tail,
		deletes: deletes,
		r: &PreviewCheckReconciler{
			Client:        cl,
			Log:           logf.Log.WithName("test"),
			Logs:          tail,
			Clock:         fakeClock,
			Recorder:      rec,
			PreviewDomain: "daddyshome.fr",
			ProbeImage:    "curlimages/curl:8.11.1",
			Enricher:      enr,
		},
	}
}

func cleanEnrichment() enrichment.Response {
	return enrichment.Response{
		Results:   []previewcheck.CVEIntel{{CVE: "CVE-2024-0001", EPSS: 0.01}},
		KEVTotal:  1200,
		EPSSTotal: 330000,
		FetchedAt: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
	}
}

func (f *fixture) reconcile() ctrl.Result {
	f.t.Helper()
	res, err := f.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "preview-check", Name: "pr-42"},
	})
	if err != nil {
		f.t.Fatalf("Reconcile: %v", err)
	}
	return res
}

// reconcileN drives the reconciler n times, so a test can express "advance the
// state machine" without hard-coding which step does what.
func (f *fixture) reconcileN(n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		f.reconcile()
	}
}

func (f *fixture) get() *previewv1.PreviewCheck {
	f.t.Helper()
	pc := &previewv1.PreviewCheck{}
	if err := f.cl.Get(context.Background(), types.NamespacedName{Namespace: "preview-check", Name: "pr-42"}, pc); err != nil {
		f.t.Fatalf("get previewcheck: %v", err)
	}
	return pc
}

func (f *fixture) jobs() []batchv1.Job {
	f.t.Helper()
	list := &batchv1.JobList{}
	if err := f.cl.List(context.Background(), list, client.InNamespace(testNS)); err != nil {
		f.t.Fatalf("list jobs: %v", err)
	}
	return list.Items
}

// completeJob marks a check Job succeeded or failed, standing in for the
// kubelet.
func (f *fixture) completeJob(check previewv1.PreviewCheckName, succeed bool) {
	f.t.Helper()
	pc := f.get()
	name := jobName(previewcheck.CheckID(string(pc.UID), pc.Name), check)
	job := &batchv1.Job{}
	if err := f.cl.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, job); err != nil {
		f.t.Fatalf("get job %s: %v", name, err)
	}
	cond := batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}
	if succeed {
		cond = batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
		job.Status.Succeeded = 1
	}
	job.Status.Conditions = append(job.Status.Conditions, cond)
	if err := f.cl.Status().Update(context.Background(), job); err != nil {
		// The fake client has no Job status subresource registered, so fall
		// back to a plain update.
		if uerr := f.cl.Update(context.Background(), job); uerr != nil {
			f.t.Fatalf("update job status: %v / %v", err, uerr)
		}
	}
}

// ---------------------------------------------------------------------------
// object builders
// ---------------------------------------------------------------------------

func newCheck(mutate ...func(*previewv1.PreviewCheck)) *previewv1.PreviewCheck {
	pc := &previewv1.PreviewCheck{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pr-42",
			Namespace: "preview-check",
			UID:       types.UID("3f2504e0-4f89-41d3-9a0c-0305e82c3301"),
		},
		Spec: previewv1.PreviewCheckSpec{PRNumber: testPR, App: testApp},
	}
	for _, m := range mutate {
		m(pc)
	}
	return pc
}

func newPreviewNamespace(mutate ...func(*corev1.Namespace)) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: testNS,
		Labels: map[string]string{
			PreviewEnvironmentLabel: "true",
			PreviewAppLabel:         testApp,
		},
	}}
	for _, m := range mutate {
		m(ns)
	}
	return ns
}

func newSettledKustomization(mutate ...func(*kustomizev1.Kustomization)) *kustomizev1.Kustomization {
	ks := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "preview-app-42",
			Namespace:   testNS,
			Generation:  3,
			Annotations: map[string]string{CredentialsPatchedAnnotation: "true"},
		},
		Status: kustomizev1.KustomizationStatus{
			LastAppliedRevision: "main@sha1:abc123def456",
		},
	}
	ks.Status.ObservedGeneration = 3
	ks.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "ReconciliationSucceeded",
		ObservedGeneration: 3, LastTransitionTime: metav1.Now(),
	}}
	for _, m := range mutate {
		m(ks)
	}
	return ks
}

func newReadyDeployment() *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: testApp, Namespace: testNS, Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: testApp, Image: testImage}},
			}},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, ObservedGeneration: 1},
	}
}

func newAppService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: testApp, Namespace: testNS},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 4533}}},
	}
}

func newVulnReport(ids ...string) *unstructured.Unstructured {
	vulns := make([]any, 0, len(ids))
	for _, id := range ids {
		vulns = append(vulns, map[string]any{"vulnerabilityID": id, "severity": "HIGH"})
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "replicaset-navidrome-abc", "namespace": testNS},
		"report": map[string]any{
			"registry":        map[string]any{"server": "ghcr.io"},
			"artifact":        map[string]any{"repository": "fredericrous/navidrome", "tag": "0.55.0"},
			"vulnerabilities": vulns,
		},
	}}
	u.SetGroupVersionKind(gvkVulnReportList.GroupVersion().WithKind("VulnerabilityReport"))
	return u
}

func newPreviewConfig(smoke *previewv1.SmokeTestConfig) *previewv1.PreviewConfig {
	return &previewv1.PreviewConfig{
		ObjectMeta: metav1.ObjectMeta{Name: testApp, Namespace: testApp},
		Spec:       previewv1.PreviewConfigSpec{SmokeTest: smoke},
	}
}

// happyObjects is a fully rendered, healthy preview.
func happyObjects() []client.Object {
	return []client.Object{
		newCheck(), newPreviewNamespace(), newSettledKustomization(),
		newReadyDeployment(), newAppService(),
		newVulnReport("CVE-2024-0001", "GHSA-xxxx-yyyy-zzzz"),
		newPreviewConfig(nil),
	}
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestReconcile_FinalizerBeforeAnySideEffect(t *testing.T) {
	f := newFixture(t, happyObjects()...)

	res := f.reconcile()
	if !res.Requeue {
		t.Errorf("the finalizer pass must requeue, got %+v", res)
	}
	pc := f.get()
	if len(pc.Finalizers) != 1 || pc.Finalizers[0] != previewCheckFinalizer {
		t.Fatalf("finalizers = %v, want [%s]", pc.Finalizers, previewCheckFinalizer)
	}
	// Nothing else may have happened yet: a CR deleted between "Job created"
	// and "finalizer added" would leak a probe pod into the preview quota.
	if pc.Status.Phase != "" {
		t.Errorf("phase = %q, want empty on the finalizer pass", pc.Status.Phase)
	}
	if len(f.jobs()) != 0 {
		t.Errorf("a Job was created before the finalizer existed")
	}
}

func TestReconcile_HappyPathWithSmokeSkipped(t *testing.T) {
	f := newFixture(t, happyObjects()...)

	f.reconcileN(4) // finalizer, start, readiness, http (creates the probe Job)
	jobs := f.jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1 probe job", len(jobs))
	}
	f.completeJob(previewv1.CheckHTTP, true)
	f.reconcileN(3) // http passes, trivy, smoke

	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckPassed {
		t.Fatalf("phase = %s (%s), want Passed", pc.Status.Phase, pc.Status.Message)
	}
	want := map[previewv1.PreviewCheckName]previewv1.CheckPhase{
		previewv1.CheckReadiness: previewv1.CheckPassed,
		previewv1.CheckHTTP:      previewv1.CheckPassed,
		previewv1.CheckTrivy:     previewv1.CheckPassed,
		previewv1.CheckSmoke:     previewv1.CheckSkipped,
	}
	if len(pc.Status.Checks) != 4 {
		t.Fatalf("checks = %+v, want 4", pc.Status.Checks)
	}
	for _, c := range pc.Status.Checks {
		if want[c.Name] != c.Phase {
			t.Errorf("check %s = %s, want %s (%s)", c.Name, c.Phase, want[c.Name], c.Message)
		}
	}
	if smoke := findCheck(pc, previewv1.CheckSmoke); smoke.Reason != previewv1.ReasonNoSmokeTest {
		t.Errorf("smoke reason = %q, want NoSmokeTest", smoke.Reason)
	}
	if pc.Status.PreviewNamespace != testNS {
		t.Errorf("previewNamespace = %q", pc.Status.PreviewNamespace)
	}
	if pc.Status.PreviewHost != "pr-42-navidrome.preview.daddyshome.fr" {
		t.Errorf("previewHost = %q", pc.Status.PreviewHost)
	}
	if pc.Status.ResolvedImage != testImage {
		t.Errorf("resolvedImage = %q, want %q", pc.Status.ResolvedImage, testImage)
	}
	if pc.Status.CompletedAt == nil {
		t.Error("completedAt must be stamped on a terminal phase")
	}
	cond := findCondition(pc)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != previewv1.ReasonAllChecksPassed {
		t.Errorf("condition = %+v", cond)
	}
	// The enricher must have been asked about the CVE only, never the GHSA id.
	if len(f.enr.got) != 1 || f.enr.got[0] != "CVE-2024-0001" {
		t.Errorf("enricher received %v, want only the CVE id", f.enr.got)
	}
}

func TestReconcile_TerminalPhaseIsNeverLeft(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.reconcileN(4)
	f.completeJob(previewv1.CheckHTTP, true)
	f.reconcileN(3)

	before := f.get()
	if before.Status.Phase != previewv1.PreviewCheckPassed {
		t.Fatalf("setup: phase = %s", before.Status.Phase)
	}
	// Break the world underneath a finished verdict; it must not change.
	if err := f.cl.Delete(context.Background(), newPreviewNamespace()); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	f.reconcileN(3)
	after := f.get()
	if after.Status.Phase != previewv1.PreviewCheckPassed {
		t.Errorf("a terminal phase flipped to %s", after.Status.Phase)
	}
	if after.Status.CompletedAt == nil || !after.Status.CompletedAt.Equal(before.Status.CompletedAt) {
		t.Errorf("completedAt moved: %v -> %v", before.Status.CompletedAt, after.Status.CompletedAt)
	}
}

func TestReconcile_GateHoldsWithNoJob(t *testing.T) {
	suspended := newSettledKustomization(func(ks *kustomizev1.Kustomization) {
		ks.Spec.Suspend = true
	})
	f := newFixture(t, newCheck(), newPreviewNamespace(), suspended, newReadyDeployment(), newAppService())

	f.reconcileN(5)
	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckPending {
		t.Errorf("phase = %s, want Pending while the gate is closed", pc.Status.Phase)
	}
	if got := conditionReason(pc); got != previewv1.ReasonKustomizationNotReady {
		t.Errorf("reason = %q, want KustomizationNotReady", got)
	}
	if len(f.jobs()) != 0 {
		t.Error("the gate must hold every side effect: a Job was created")
	}
	if len(pc.Status.Checks) != 0 {
		t.Errorf("no check may run behind a closed gate, got %+v", pc.Status.Checks)
	}
}

func TestReconcile_GateClosingMidCheckKeepsRunning(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.reconcileN(4) // through readiness and into http
	if findCheck(f.get(), previewv1.CheckReadiness) == nil {
		t.Fatal("setup: readiness never ran")
	}

	// The operator re-suspends the Kustomization on a PreviewConfig hash
	// change. A re-render is not a verdict.
	ks := &kustomizev1.Kustomization{}
	if err := f.cl.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "preview-app-42"}, ks); err != nil {
		t.Fatalf("get kustomization: %v", err)
	}
	ks.Spec.Suspend = true
	if err := f.cl.Update(context.Background(), ks); err != nil {
		t.Fatalf("suspend kustomization: %v", err)
	}

	f.reconcile()
	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckRunning {
		t.Errorf("phase = %s, want Running: a gate closing mid-check is not a failure", pc.Status.Phase)
	}
	if got := conditionReason(pc); got != previewv1.ReasonKustomizationNotReady {
		t.Errorf("reason = %q, want KustomizationNotReady", got)
	}
	if readiness := findCheck(pc, previewv1.CheckReadiness); readiness.Phase != previewv1.CheckPassed {
		t.Errorf("a finished check must survive the gate closing, got %s", readiness.Phase)
	}
}

func TestReconcile_RevisionMismatch(t *testing.T) {
	check := newCheck(func(pc *previewv1.PreviewCheck) { pc.Spec.Revision = "deadbeef" })
	f := newFixture(t, check, newPreviewNamespace(), newSettledKustomization(), newReadyDeployment(), newAppService())

	f.reconcileN(3)
	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckPending {
		t.Errorf("phase = %s, want Pending before the deadline", pc.Status.Phase)
	}
	if got := conditionReason(pc); got != previewv1.ReasonRevisionMismatch {
		t.Errorf("reason = %q, want RevisionMismatch", got)
	}
	if len(f.jobs()) != 0 {
		t.Error("a revision mismatch must not run anything")
	}

	// At the deadline a preview stuck on the wrong commit is a negative
	// verdict, not an inconclusive one: the caller's change was never applied.
	f.clock.SetTime(f.clock.Now().Add(31 * time.Minute))
	f.reconcile()
	pc = f.get()
	if pc.Status.Phase != previewv1.PreviewCheckFailed {
		t.Errorf("phase = %s, want Failed at the deadline", pc.Status.Phase)
	}
}

func TestReconcile_AppMismatchIsTerminal(t *testing.T) {
	ns := newPreviewNamespace(func(ns *corev1.Namespace) {
		ns.Labels[PreviewAppLabel] = "someone-elses-app"
	})
	f := newFixture(t, newCheck(), ns, newSettledKustomization())

	f.reconcileN(3)
	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckFailed {
		t.Fatalf("phase = %s, want Failed", pc.Status.Phase)
	}
	if got := conditionReason(pc); got != previewv1.ReasonAppMismatch {
		t.Errorf("reason = %q, want AppMismatch", got)
	}
	// The whole point: spec.app never selects a namespace or a PreviewConfig.
	if pc.Status.PreviewNamespace != "" {
		t.Errorf("a mismatched namespace must not be recorded as observed, got %q", pc.Status.PreviewNamespace)
	}
}

func TestReconcile_NamespaceAbsentIsPendingThenExpired(t *testing.T) {
	f := newFixture(t, newCheck())

	// The CR is created at PR-open, but the ResourceSetInputProvider polls
	// every minute and only sees PRs that already carry `preview-ready`. An
	// absent namespace is the NORMAL state for the first minutes.
	f.reconcileN(3)
	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckPending {
		t.Errorf("phase = %s, want Pending while the preview is still rendering", pc.Status.Phase)
	}
	if got := conditionReason(pc); got != previewv1.ReasonNamespaceGone {
		t.Errorf("reason = %q, want NamespaceGone", got)
	}

	// Never rendered by the deadline: inconclusive. The caller leaves the PR
	// open and escalates rather than concluding the change is bad.
	f.clock.SetTime(f.clock.Now().Add(31 * time.Minute))
	f.reconcile()
	pc = f.get()
	if pc.Status.Phase != previewv1.PreviewCheckExpired {
		t.Errorf("phase = %s, want Expired for a preview that never rendered", pc.Status.Phase)
	}
}

func TestReconcile_ObservedNamespaceDeletedIsFailed(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.reconcileN(3)
	if f.get().Status.PreviewNamespace != testNS {
		t.Fatal("setup: the namespace was never observed")
	}

	if err := f.cl.Delete(context.Background(), newPreviewNamespace()); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	f.reconcile()

	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckFailed {
		t.Errorf("phase = %s, want Failed: an observed preview that vanished is a verdict", pc.Status.Phase)
	}
	if got := conditionReason(pc); got != previewv1.ReasonNamespaceGone {
		t.Errorf("reason = %q, want NamespaceGone", got)
	}
}

func TestReconcile_TerminatingNamespace(t *testing.T) {
	now := metav1.Now()
	terminating := newPreviewNamespace(func(ns *corev1.Namespace) {
		ns.DeletionTimestamp = &now
		ns.Finalizers = []string{"kubernetes"}
	})
	f := newFixture(t, newCheck(), terminating)

	// Never observed, so still inconclusive rather than a verdict.
	f.reconcileN(3)
	if got := f.get().Status.Phase; got != previewv1.PreviewCheckPending {
		t.Errorf("phase = %s, want Pending for a terminating namespace never observed", got)
	}
}

func TestReconcile_NamespaceWithoutPreviewLabelRunsNothing(t *testing.T) {
	plain := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}}
	f := newFixture(t, newCheck(), plain, newSettledKustomization(), newReadyDeployment(), newAppService())

	f.reconcileN(4)
	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckPending {
		t.Errorf("phase = %s, want Pending", pc.Status.Phase)
	}
	if len(f.jobs()) != 0 {
		t.Error("a namespace without preview-environment=true must never host a check Job")
	}
}

func TestEnsureJob_RefusesOutsideAPreviewNamespace(t *testing.T) {
	f := newFixture(t, newCheck())
	pc := newCheck()
	env := &checkEnv{
		nsName: testNS,
		app:    testApp,
		id:     "abc123",
		ns:     &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}}, // no label
	}

	job, step, err := f.r.ensureJob(context.Background(), env, f.r.probeJob(env, pc, pc.Spec.Defaulted(), "http://x"))
	if err == nil {
		t.Fatal("ensureJob must refuse a namespace lacking preview-environment=true")
	}
	if job != nil || step != nil {
		t.Errorf("a refusal is an error, not a verdict: job=%v step=%v", job, step)
	}
	if !strings.Contains(err.Error(), PreviewEnvironmentLabel) {
		t.Errorf("the error must name the guard, got %v", err)
	}
	if len(f.jobs()) != 0 {
		t.Error("a Job was created anyway")
	}
}

func TestReconcile_RunningCheckReconciledTwiceCreatesOneJob(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.reconcileN(4) // creates the probe Job
	if len(f.jobs()) != 1 {
		t.Fatalf("setup: jobs = %d", len(f.jobs()))
	}
	// The Job is still running; the ladder polls it. Each poll must find the
	// existing Job, not race a second one into the preview's quota.
	f.reconcileN(3)
	if got := len(f.jobs()); got != 1 {
		t.Errorf("jobs = %d after polling, want 1", got)
	}
	if phase := findCheck(f.get(), previewv1.CheckHTTP).Phase; phase != previewv1.CheckRunning {
		t.Errorf("http check = %s, want Running", phase)
	}
}

func TestReconcile_TerminalCheckNeverRecreatesItsJob(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.reconcileN(4)
	f.completeJob(previewv1.CheckHTTP, true)
	f.reconcileN(3)
	if f.get().Status.Phase != previewv1.PreviewCheckPassed {
		t.Fatalf("setup: phase = %s", f.get().Status.Phase)
	}

	// Delete the finished Job the way ttlSecondsAfterFinished would, then keep
	// reconciling. A terminal check must not notice.
	jobs := f.jobs()
	for i := range jobs {
		if err := f.cl.Delete(context.Background(), &jobs[i]); err != nil {
			t.Fatalf("delete job: %v", err)
		}
	}
	f.reconcileN(3)
	if got := len(f.jobs()); got != 0 {
		t.Errorf("a terminal check re-created its Job: %d jobs", got)
	}
}

func TestReconcile_ProbeJobFailedCarriesTheLogTail(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.tail.out = "probe: status=500\nprobe: unexpected status 500, expected one of 200 302"

	f.reconcileN(4)
	f.completeJob(previewv1.CheckHTTP, false)
	f.reconcile()

	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckFailed {
		t.Fatalf("phase = %s, want Failed", pc.Status.Phase)
	}
	// The container printed a status the expectStatus set does not accept, so
	// this is a verdict about the app — not "the probe could not run".
	if got := conditionReason(pc); got != previewv1.ReasonUnexpectedStatus {
		t.Errorf("reason = %q, want UnexpectedStatus", got)
	}
	httpCheck := findCheck(pc, previewv1.CheckHTTP)
	if httpCheck.Details["httpStatus"] != "500" {
		t.Errorf("details = %v, want httpStatus 500", httpCheck.Details)
	}
	if !strings.Contains(httpCheck.Message, "unexpected status 500") {
		t.Errorf("message must carry the log tail, got %q", httpCheck.Message)
	}
	if f.tail.calls == 0 {
		t.Error("the log tailer was never called")
	}
}

func TestReconcile_ProbeJobFailedWithoutAStatusIsProbeJobFailed(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.tail.out = "probe: request to http://navidrome... failed"

	f.reconcileN(4)
	f.completeJob(previewv1.CheckHTTP, false)
	f.reconcile()

	if got := conditionReason(f.get()); got != previewv1.ReasonProbeJobFailed {
		t.Errorf("reason = %q, want ProbeJobFailed when no status was ever obtained", got)
	}
}

func TestReconcile_KEVExceededIsTerminal(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.enr.resp = enrichment.Response{
		Results:   []previewcheck.CVEIntel{{CVE: "CVE-2024-0001", KEV: true, EPSS: 0.02}},
		KEVTotal:  1200,
		FetchedAt: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
	}

	f.reconcileN(4)
	f.completeJob(previewv1.CheckHTTP, true)
	f.reconcileN(2)

	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckFailed {
		t.Fatalf("phase = %s (%s), want Failed", pc.Status.Phase, pc.Status.Message)
	}
	if got := conditionReason(pc); got != previewv1.ReasonKEVExceeded {
		t.Errorf("reason = %q, want KEVExceeded", got)
	}
	// Fail-fast: smoke must never have run.
	if findCheck(pc, previewv1.CheckSmoke) != nil {
		t.Error("checks after a failure must not run")
	}
	before := pc.Status.Checks
	f.reconcileN(3)
	if got := len(f.get().Status.Checks); got != len(before) {
		t.Errorf("a terminal CR kept working: %d checks, was %d", got, len(before))
	}
}

func TestReconcile_ScanMissingAtDeadlineIsExpired(t *testing.T) {
	objects := []client.Object{
		newCheck(), newPreviewNamespace(), newSettledKustomization(),
		newReadyDeployment(), newAppService(), newPreviewConfig(nil),
		// No VulnerabilityReport at all.
	}
	f := newFixture(t, objects...)

	f.reconcileN(4)
	f.completeJob(previewv1.CheckHTTP, true)
	f.reconcileN(2)

	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckRunning {
		t.Fatalf("phase = %s, want Running while the scan is pending", pc.Status.Phase)
	}
	if got := conditionReason(pc); got != previewv1.ReasonScanPending {
		t.Errorf("reason = %q, want ScanPending", got)
	}
	if f.enr.calls != 0 {
		t.Error("the enricher must not be called before a scan exists")
	}

	f.clock.SetTime(f.clock.Now().Add(31 * time.Minute))
	f.reconcile()
	pc = f.get()
	// A scan that never arrived means the change was never judged. Expired,
	// not Failed: the caller keeps the PR open.
	if pc.Status.Phase != previewv1.PreviewCheckExpired {
		t.Errorf("phase = %s, want Expired", pc.Status.Phase)
	}
	if got := conditionReason(pc); got != previewv1.ReasonScanMissing {
		t.Errorf("reason = %q, want ScanMissing", got)
	}
	if trivy := findCheck(pc, previewv1.CheckTrivy); trivy.Phase != previewv1.CheckFailed {
		t.Errorf("the check that ran out of road is Failed (there is no Expired check phase), got %s", trivy.Phase)
	}
}

func TestReconcile_EmptyCVESetPassesWithoutCallingTheEnricher(t *testing.T) {
	objects := happyObjects()
	// Only non-CVE ids: posting an empty list would earn a 400 the ladder
	// reads as terminal, so a clean image would fail closed.
	objects[5] = newVulnReport("GHSA-xxxx-yyyy-zzzz", "DSA-5555-1")
	f := newFixture(t, objects...)

	f.reconcileN(4)
	f.completeJob(previewv1.CheckHTTP, true)
	f.reconcileN(3)

	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckPassed {
		t.Fatalf("phase = %s (%s), want Passed", pc.Status.Phase, pc.Status.Message)
	}
	if f.enr.calls != 0 {
		t.Errorf("the enricher was called %d times for an empty CVE set", f.enr.calls)
	}
	trivy := findCheck(pc, previewv1.CheckTrivy)
	if trivy.Details["nonCveIds"] == "" {
		t.Errorf("the dropped ids must be published as detail, got %v", trivy.Details)
	}
}

func TestReconcile_EnrichmentUnavailableRetriesThenExpires(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.enr.err = fmt.Errorf("dial tcp: connection refused")

	f.reconcileN(4)
	f.completeJob(previewv1.CheckHTTP, true)
	f.reconcileN(2)

	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckRunning {
		t.Fatalf("phase = %s, want Running: a transient enrichment failure is retried", pc.Status.Phase)
	}
	if got := conditionReason(pc); got != previewv1.ReasonEnrichmentUnavailable {
		t.Errorf("reason = %q, want EnrichmentUnavailable", got)
	}

	f.clock.SetTime(f.clock.Now().Add(31 * time.Minute))
	f.reconcile()
	pc = f.get()
	// Never pass-by-default, and never call it a bad change either: we simply
	// could not look.
	if pc.Status.Phase != previewv1.PreviewCheckExpired {
		t.Errorf("phase = %s, want Expired", pc.Status.Phase)
	}
	if trivy := findCheck(pc, previewv1.CheckTrivy); trivy.Phase != previewv1.CheckFailed || trivy.Reason != previewv1.ReasonEnrichmentUnavailable {
		t.Errorf("trivy check = %s/%s", trivy.Phase, trivy.Reason)
	}
}

func TestReconcile_StaleEnrichmentIsNotAPass(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	// A perfectly well-formed 200 over an empty cache: every id comes back
	// unknown, unknown counts as clean, and a vulnerable image sails through.
	f.enr.resp = enrichment.Response{KEVTotal: 0, Unknown: []string{"CVE-2024-0001"}}

	f.reconcileN(4)
	f.completeJob(previewv1.CheckHTTP, true)
	f.reconcileN(2)

	pc := f.get()
	if pc.Status.Phase == previewv1.PreviewCheckPassed {
		t.Fatal("an empty enrichment cache must never produce a pass")
	}
	if got := conditionReason(pc); got != previewv1.ReasonEnrichmentUnavailable {
		t.Errorf("reason = %q, want EnrichmentUnavailable", got)
	}
}

func TestReconcile_NilEnricherFailsClosed(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.r.Enricher = nil

	f.reconcileN(4)
	f.completeJob(previewv1.CheckHTTP, true)
	f.reconcileN(2)

	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckExpired {
		t.Errorf("phase = %s, want Expired with no enrichment endpoint configured", pc.Status.Phase)
	}
	if got := conditionReason(pc); got != previewv1.ReasonEnrichmentUnavailable {
		t.Errorf("reason = %q, want EnrichmentUnavailable", got)
	}
}

func TestReconcile_SmokeTestReadsThePreviewConfigFromProduction(t *testing.T) {
	smoke := &previewv1.SmokeTestConfig{
		Image:          "ghcr.io/fredericrous/smoke:1",
		Command:        []string{"/bin/sh", "-c"},
		Args:           []string{"curl -fsS $PREVIEW_URL/rest/ping.view"},
		Env:            map[string]string{"B": "2", "A": "1"},
		TimeoutSeconds: 120,
	}
	objects := happyObjects()
	objects[6] = newPreviewConfig(smoke)
	// A PreviewConfig in the PREVIEW namespace must be ignored entirely: it is
	// rendered from the pull request's own tree, so honouring it would let a PR
	// rewrite the test that judges it.
	attacker := newPreviewConfig(&previewv1.SmokeTestConfig{Image: "evil:latest"})
	attacker.Namespace = testNS
	objects = append(objects, attacker)

	f := newFixture(t, objects...)
	f.reconcileN(4)
	f.completeJob(previewv1.CheckHTTP, true)
	f.reconcileN(3) // http passes, trivy passes, smoke creates its Job

	var smokeJob *batchv1.Job
	for _, j := range f.jobs() {
		if j.Labels[CheckNameLabel] == string(previewv1.CheckSmoke) {
			job := j
			smokeJob = &job
		}
	}
	if smokeJob == nil {
		t.Fatalf("no smoke job; jobs = %+v, status = %+v", f.jobs(), f.get().Status)
	}
	c := smokeJob.Spec.Template.Spec.Containers[0]
	if c.Image != "ghcr.io/fredericrous/smoke:1" {
		t.Errorf("smoke image = %q — the PREVIEW namespace's PreviewConfig must never be read", c.Image)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["PREVIEW_URL"] != "http://navidrome.preview-pr-42.svc.cluster.local:4533" {
		t.Errorf("PREVIEW_URL = %q", env["PREVIEW_URL"])
	}
	if env["PREVIEW_NAMESPACE"] != testNS || env["APP_NAME"] != testApp || env["PR_NUMBER"] != "42" {
		t.Errorf("env = %v", env)
	}
	if _, present := env["PREVIEW_HOST_URL"]; present {
		t.Error("PREVIEW_HOST_URL must not be injected: the edge listener demands a client certificate")
	}
	if env["A"] != "1" || env["B"] != "2" {
		t.Errorf("user env missing: %v", env)
	}
	if smokeJob.Spec.Template.Spec.AutomountServiceAccountToken == nil || *smokeJob.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be false: the preview namespace holds a repo-write git token")
	}
	if *smokeJob.Spec.ActiveDeadlineSeconds != 120 {
		t.Errorf("activeDeadlineSeconds = %d, want 120", *smokeJob.Spec.ActiveDeadlineSeconds)
	}
	if smokeJob.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", smokeJob.Spec.Template.Spec.RestartPolicy)
	}
	if *smokeJob.Spec.TTLSecondsAfterFinished != 900 {
		t.Errorf("ttlSecondsAfterFinished = %d, want 900", *smokeJob.Spec.TTLSecondsAfterFinished)
	}
	sc := c.SecurityContext
	if sc == nil || *sc.AllowPrivilegeEscalation || len(sc.Capabilities.Drop) != 1 || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("smoke security context = %+v", sc)
	}
	// No ambient opt-out: preview namespaces carry both a STRICT and a
	// PERMISSIVE PeerAuthentication, and an out-of-mesh Job loses to the former.
	if _, present := smokeJob.Annotations["ambient.istio.io/redirection"]; present {
		t.Error("check Jobs must stay in the mesh")
	}
}

func TestReconcile_SmokeJobFailed(t *testing.T) {
	objects := happyObjects()
	objects[6] = newPreviewConfig(&previewv1.SmokeTestConfig{Image: "smoke:1"})
	f := newFixture(t, objects...)
	f.tail.out = "assertion failed: expected 3 albums, got 0"

	f.reconcileN(4)
	f.completeJob(previewv1.CheckHTTP, true)
	f.reconcileN(3)
	f.completeJob(previewv1.CheckSmoke, false)
	f.reconcile()

	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckFailed {
		t.Fatalf("phase = %s, want Failed", pc.Status.Phase)
	}
	if got := conditionReason(pc); got != previewv1.ReasonSmokeJobFailed {
		t.Errorf("reason = %q, want SmokeJobFailed", got)
	}
	if !strings.Contains(findCheck(pc, previewv1.CheckSmoke).Message, "expected 3 albums") {
		t.Errorf("the smoke failure must carry the log tail: %q", findCheck(pc, previewv1.CheckSmoke).Message)
	}
}

func TestReconcile_ProbeJobSpec(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.reconcileN(4)

	jobs := f.jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d", len(jobs))
	}
	job := jobs[0]
	if job.Name != "preview-check-3f2504e04f89-http" {
		t.Errorf("job name = %q", job.Name)
	}
	if job.Labels[CheckJobLabel] != "pr-42" {
		t.Errorf("labels = %v", job.Labels)
	}
	if len(job.OwnerReferences) != 0 {
		t.Error("no ownerReferences: a cross-namespace ownerRef makes the GC reap the Job immediately")
	}
	if *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %d, want 0: the container's exit code IS the verdict", *job.Spec.BackoffLimit)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.TTLSecondsAfterFinished != 900 {
		t.Errorf("deadline/ttl = %v/%v", job.Spec.ActiveDeadlineSeconds, job.Spec.TTLSecondsAfterFinished)
	}
	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever || *pod.AutomountServiceAccountToken {
		t.Errorf("pod = %s / automount %v", pod.RestartPolicy, *pod.AutomountServiceAccountToken)
	}
	c := pod.Containers[0]
	if !*c.SecurityContext.ReadOnlyRootFilesystem || !*c.SecurityContext.RunAsNonRoot || *c.SecurityContext.RunAsUser != 100 {
		t.Errorf("probe security context = %+v", c.SecurityContext)
	}
	if c.Resources.Requests.Cpu().String() != "10m" || c.Resources.Limits.Memory().String() != "64Mi" {
		t.Errorf("probe resources = %+v", c.Resources)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["TARGET_URL"] != "http://navidrome.preview-pr-42.svc.cluster.local:4533/" {
		t.Errorf("TARGET_URL = %q", env["TARGET_URL"])
	}
	if env["EXPECT_STATUS"] != "200 201 202 203 204 301 302 303 307 308" {
		t.Errorf("EXPECT_STATUS = %q — the default must span 3xx so OIDC redirects pass", env["EXPECT_STATUS"])
	}
}

func TestReconcile_HTTPTargetPrefersTheHTTPRouteBackend(t *testing.T) {
	route := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "preview-42", "namespace": testNS},
		"spec": map[string]any{"rules": []any{map[string]any{
			"backendRefs": []any{map[string]any{"name": "navidrome-web", "port": int64(8080)}},
		}}},
	}}
	route.SetGroupVersionKind(gvkHTTPRoute)

	objects := append(happyObjects(), route, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "navidrome-web", Namespace: testNS},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
	})
	f := newFixture(t, objects...)
	f.reconcileN(4)

	c := f.jobs()[0].Spec.Template.Spec.Containers[0]
	for _, e := range c.Env {
		if e.Name == "TARGET_URL" && e.Value != "http://navidrome-web.preview-pr-42.svc.cluster.local:8080/" {
			t.Errorf("TARGET_URL = %q — the HTTPRoute backend is what a human would reach", e.Value)
		}
	}
}

func TestReconcile_TTLExpiry(t *testing.T) {
	f := newFixture(t, newCheck(), newPreviewNamespace(), newSettledKustomization())
	f.reconcileN(2)
	if f.get().Status.ExpiresAt == nil {
		t.Fatal("expiresAt must be stamped on the first pass")
	}

	f.clock.SetTime(f.clock.Now().Add(3 * time.Hour)) // past the 7200 s default
	f.reconcile()

	pc := f.get()
	if pc.Status.Phase != previewv1.PreviewCheckExpired {
		t.Errorf("phase = %s, want Expired past the TTL", pc.Status.Phase)
	}
	if got := conditionReason(pc); got != previewv1.ReasonTimeout {
		t.Errorf("reason = %q, want Timeout", got)
	}

	// The self-delete backstop only fires long after the TTL, and only on a
	// terminal CR. The caller is expected to delete its own.
	f.reconcile()
	if err := f.cl.Get(context.Background(), types.NamespacedName{Namespace: "preview-check", Name: "pr-42"}, &previewv1.PreviewCheck{}); err != nil {
		t.Fatalf("the CR must survive its TTL: %v", err)
	}
	f.clock.SetTime(f.clock.Now().Add(25 * time.Hour))
	f.reconcile() // issues the Delete, which the finalizer turns into a deletionTimestamp
	f.reconcile() // runs teardown and releases the finalizer
	if err := f.cl.Get(context.Background(), types.NamespacedName{Namespace: "preview-check", Name: "pr-42"}, &previewv1.PreviewCheck{}); !apierrors.IsNotFound(err) {
		t.Errorf("the backstop must delete a terminal CR 24 h past its TTL, got %v", err)
	}
}

func TestTeardown_DeletesJobsWithBackgroundPropagationAndIsIdempotent(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.reconcileN(4)
	if len(f.jobs()) != 1 {
		t.Fatalf("setup: jobs = %d", len(f.jobs()))
	}

	pc := f.get()
	now := metav1.Now()
	pc.DeletionTimestamp = &now
	if err := f.cl.Delete(context.Background(), pc); err != nil {
		t.Fatalf("delete previewcheck: %v", err)
	}
	*f.deletes = nil

	f.reconcile()

	if got := len(f.jobs()); got != 0 {
		t.Errorf("jobs = %d after teardown, want 0", got)
	}
	// Jobs default to ORPHAN propagation: without Background the probe pod
	// keeps running and keeps burning the preview's quota.
	found := false
	for _, d := range *f.deletes {
		if strings.HasPrefix(d.name, "preview-check-") {
			found = true
			if d.options.PropagationPolicy == nil || *d.options.PropagationPolicy != metav1.DeletePropagationBackground {
				t.Errorf("job delete propagation = %v, want Background", d.options.PropagationPolicy)
			}
		}
	}
	if !found {
		t.Error("no check Job delete was observed")
	}

	// Idempotent: a second teardown against an already-empty (or already gone)
	// preview must not wedge the finalizer.
	if err := f.r.teardown(context.Background(), logf.Log, pc); err != nil {
		t.Errorf("second teardown: %v", err)
	}
	if err := f.cl.Delete(context.Background(), newPreviewNamespace()); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	if err := f.r.teardown(context.Background(), logf.Log, pc); err != nil {
		t.Errorf("teardown against a missing namespace must succeed, got %v", err)
	}
}

func TestTeardown_RemovesTheFinalizer(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.reconcileN(2)

	pc := f.get()
	if err := f.cl.Delete(context.Background(), pc); err != nil {
		t.Fatalf("delete: %v", err)
	}
	f.reconcile()

	err := f.cl.Get(context.Background(), types.NamespacedName{Namespace: "preview-check", Name: "pr-42"}, &previewv1.PreviewCheck{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("the finalizer must be removed so the CR can go, got %v", err)
	}
}

func TestOrderedChecks_FixedOrderRegardlessOfSpec(t *testing.T) {
	got := orderedChecks([]previewv1.PreviewCheckName{
		previewv1.CheckSmoke, previewv1.CheckTrivy, previewv1.CheckReadiness,
	})
	want := []previewv1.PreviewCheckName{previewv1.CheckReadiness, previewv1.CheckTrivy, previewv1.CheckSmoke}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("orderedChecks = %v, want %v", got, want)
	}
	if len(orderedChecks(nil)) != 0 {
		t.Error("an empty selection selects nothing")
	}
}

func TestTerminalPhaseFor(t *testing.T) {
	// The Failed/Expired split is the caller's whole contract: Failed means
	// "judged bad, close the PR", Expired means "never judged, leave it open".
	inconclusive := []string{
		previewv1.ReasonScanMissing, previewv1.ReasonEnrichmentUnavailable,
		previewv1.ReasonQuotaExceeded, previewv1.ReasonTimeout,
	}
	for _, reason := range inconclusive {
		if got := terminalPhaseFor(reason); got != previewv1.PreviewCheckExpired {
			t.Errorf("terminalPhaseFor(%s) = %s, want Expired", reason, got)
		}
	}
	verdicts := []string{
		previewv1.ReasonPodsNotReady, previewv1.ReasonRevisionMismatch,
		previewv1.ReasonUnexpectedStatus, previewv1.ReasonProbeJobFailed,
		previewv1.ReasonKEVExceeded, previewv1.ReasonEPSSExceeded,
		previewv1.ReasonSmokeJobFailed, previewv1.ReasonAppMismatch,
		previewv1.ReasonInvalidThreshold, previewv1.ReasonNamespaceGone,
	}
	for _, reason := range verdicts {
		if got := terminalPhaseFor(reason); got != previewv1.PreviewCheckFailed {
			t.Errorf("terminalPhaseFor(%s) = %s, want Failed", reason, got)
		}
	}
}

func TestClassifyForbidden(t *testing.T) {
	gr := schema.GroupResource{Group: "batch", Resource: "jobs"}

	quota := apierrors.NewForbidden(gr, "probe", fmt.Errorf("exceeded quota: preview-quota, requested: pods=1, used: pods=20, limited: pods=20"))
	if got := classifyForbidden(quota); got != forbiddenQuota {
		t.Errorf("quota 403 classified as %v", got)
	}

	terminating := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: 403, Reason: metav1.StatusReasonForbidden,
		Message: "unable to create new content in namespace preview-pr-42 because it is being terminated",
		Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{
			Type:    corev1.NamespaceTerminatingCause,
			Message: "namespace preview-pr-42 is being terminated",
		}}},
	}}
	if got := classifyForbidden(terminating); got != forbiddenTerminating {
		t.Errorf("terminating 403 classified as %v", got)
	}
	if !namespaceUnavailable(terminating) {
		t.Error("a terminating namespace must be treated as gone on teardown")
	}

	rbac := apierrors.NewForbidden(gr, "probe", fmt.Errorf(`jobs is forbidden: User "system:serviceaccount:x:y" cannot create resource "jobs"`))
	if got := classifyForbidden(rbac); got != forbiddenOther {
		t.Errorf("an RBAC gap classified as %v — it must surface as a reconcile error, never a verdict", got)
	}
	if namespaceUnavailable(rbac) {
		t.Error("an RBAC gap must not be swallowed as a missing namespace")
	}
	if got := classifyForbidden(apierrors.NewNotFound(gr, "probe")); got != forbiddenOther {
		t.Errorf("a NotFound classified as %v", got)
	}
}

func TestParseProbeStatus(t *testing.T) {
	if code, ok := parseProbeStatus("probe: status=302\nprobe: accepted"); !ok || code != 302 {
		t.Errorf("= %d, %v", code, ok)
	}
	// The LAST status wins: a retried probe prints more than one.
	if code, _ := parseProbeStatus("status=500\nstatus=200"); code != 200 {
		t.Errorf("= %d, want the last status", code)
	}
	if _, ok := parseProbeStatus("probe: request failed"); ok {
		t.Error("no status must be reported as absent, not as zero")
	}
	if _, ok := parseProbeStatus(""); ok {
		t.Error("an empty tail has no status")
	}
}

func TestIsInfraService(t *testing.T) {
	// A preview namespace also holds the CNPG clone, a Redis and an S3 proxy;
	// probing Postgres over HTTP would look like an app failure.
	for _, name := range []string{"pg-preview-42-rw", "pg-preview-42-ro", "pg-preview-42-r", "pg-preview-42-any", "redis-preview", "s3proxy"} {
		if !isInfraService(name) {
			t.Errorf("%s must be filtered out", name)
		}
	}
	for _, name := range []string{"navidrome", "navidrome-web", "app"} {
		if isInfraService(name) {
			t.Errorf("%s must not be filtered out", name)
		}
	}
}

func TestKustomizationSettled(t *testing.T) {
	settled, _, _, _ := kustomizationSettled(newSettledKustomization(), "")
	if !settled {
		t.Error("a settled Kustomization must pass the gate")
	}
	settled, _, _, _ = kustomizationSettled(newSettledKustomization(), "sha1:abc123def456")
	if !settled {
		t.Error("a matching revision suffix must pass")
	}
	settled, reason, _, _ := kustomizationSettled(newSettledKustomization(), "deadbeef")
	if settled || reason != previewv1.ReasonRevisionMismatch {
		t.Errorf("a non-matching revision = %v/%s", settled, reason)
	}

	// The operator's annotation machine keeps the Kustomization suspended until
	// credentials are patched in; a Ready=True from before that patch describes
	// a preview pointing at nothing.
	noCreds := newSettledKustomization(func(ks *kustomizev1.Kustomization) { ks.Annotations = nil })
	if settled, _, _, _ := kustomizationSettled(noCreds, ""); settled {
		t.Error("credentials-patched must gate")
	}
	stale := newSettledKustomization(func(ks *kustomizev1.Kustomization) { ks.Generation = 4 })
	if settled, _, _, _ := kustomizationSettled(stale, ""); settled {
		t.Error("an unobserved generation must gate")
	}
	failing := newSettledKustomization(func(ks *kustomizev1.Kustomization) {
		ks.Status.Conditions[0].Status = metav1.ConditionFalse
		ks.Status.Conditions[0].Reason = "BuildFailed"
	})
	settled, _, _, readyFalse := kustomizationSettled(failing, "")
	if settled || !readyFalse {
		t.Errorf("Ready=False must gate AND be reported as a real negative verdict: %v/%v", settled, readyFalse)
	}
}

// ---------------------------------------------------------------------------

func findCondition(pc *previewv1.PreviewCheck) *metav1.Condition {
	for i := range pc.Status.Conditions {
		if pc.Status.Conditions[i].Type == previewv1.ConditionSucceeded {
			return &pc.Status.Conditions[i]
		}
	}
	return nil
}

func conditionReason(pc *previewv1.PreviewCheck) string {
	if c := findCondition(pc); c != nil {
		return c.Reason
	}
	return ""
}

func TestReconcile_EmitsAnEventAtEachTransition(t *testing.T) {
	f := newFixture(t, happyObjects()...)
	f.reconcileN(4)
	f.completeJob(previewv1.CheckHTTP, true)
	f.reconcileN(3)

	var events []string
	for {
		select {
		case e := <-f.rec.Events:
			events = append(events, e)
			continue
		default:
		}
		break
	}
	joined := strings.Join(events, "\n")
	for _, want := range []string{previewv1.ReasonPending, previewv1.ReasonRunning, previewv1.ReasonAllChecksPassed} {
		if !strings.Contains(joined, want) {
			t.Errorf("no event for %s in:\n%s", want, joined)
		}
	}
	// A transition, not a heartbeat: the same phase/reason twice in a row must
	// not mint a second event.
	before := len(events)
	f.reconcileN(3)
	select {
	case e := <-f.rec.Events:
		t.Errorf("a terminal, unchanged CR emitted another event (%d before): %s", before, e)
	default:
	}
}
