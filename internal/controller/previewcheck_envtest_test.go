package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	previewv1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

// The envtest suite exists for the things a fake client structurally cannot
// answer: whether the API server ACCEPTS the CRD at all (the CEL cost estimate
// on the per-field immutability rules is a real budget, and a rejected CRD
// fails the HelmRelease outright under `crds: CreateReplace`), and whether the
// defaulting and validation we wrote in markers actually behave as written.
//
// It is skipped when KUBEBUILDER_ASSETS is unset so `make test-unit` runs the
// same package without a control plane; `make test-integration` provides them.
var envtestClient client.Client

func TestMain(m *testing.M) {
	os.Exit(func() int {
		if os.Getenv("KUBEBUILDER_ASSETS") == "" {
			return m.Run()
		}
		cl, stop, err := startEnvtest()
		if err != nil {
			fmt.Fprintf(os.Stderr, "envtest: %v\n", err)
			return 1
		}
		defer stop()
		envtestClient = cl
		return m.Run()
	}())
}

func startEnvtest() (client.Client, func(), error) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	// A CRD the API server refuses — an unbounded CEL rule, a list without the
	// maxItems the cost estimator needs — fails HERE, in CI, rather than at
	// deploy time inside a HelmRelease.
	cfg, err := env.Start()
	if err != nil {
		return nil, nil, fmt.Errorf("start control plane (is the CRD acceptable?): %w", err)
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(previewv1.AddToScheme(scheme))

	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = env.Stop()
		return nil, nil, fmt.Errorf("build client: %w", err)
	}
	return cl, func() { _ = env.Stop() }, nil
}

func requireEnvtest(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `make test-integration`")
	}
	if envtestClient == nil {
		t.Fatal("envtest client was not initialised")
	}
	return envtestClient
}

func envtestCheck(t *testing.T, mutate ...func(*previewv1.PreviewCheck)) *previewv1.PreviewCheck {
	t.Helper()
	pc := &previewv1.PreviewCheck{
		ObjectMeta: metav1.ObjectMeta{
			Name:      strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-")),
			Namespace: "default",
		},
		Spec: previewv1.PreviewCheckSpec{PRNumber: 42, App: "navidrome"},
	}
	for _, m := range mutate {
		m(pc)
	}
	return pc
}

func TestEnvtest_DefaultsMaterialise(t *testing.T) {
	cl := requireEnvtest(t)
	ctx := context.Background()

	pc := envtestCheck(t)
	if err := cl.Create(ctx, pc); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = cl.Delete(ctx, pc) })

	got := &previewv1.PreviewCheck{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: pc.Name}, got); err != nil {
		t.Fatalf("get: %v", err)
	}

	if len(got.Spec.Checks) != 4 {
		t.Errorf("checks = %v, want all four", got.Spec.Checks)
	}
	if got.Spec.HTTPPath != "/" {
		t.Errorf("httpPath = %q", got.Spec.HTTPPath)
	}
	if len(got.Spec.ExpectStatus) != 10 {
		t.Errorf("expectStatus = %v, want the 2xx+3xx default", got.Spec.ExpectStatus)
	}
	if got.Spec.TimeoutSeconds != 1800 || got.Spec.TTLSeconds != 7200 {
		t.Errorf("timeout/ttl = %d/%d", got.Spec.TimeoutSeconds, got.Spec.TTLSeconds)
	}

	// The whole reason `thresholds` carries BOTH a `{}` default on the parent
	// and leaf defaults inside it: a typed client serialises the absent struct
	// as `{}`, the parent default never fires, and without leaf defaults the
	// thresholds would silently be 0/0 — the strictest possible, failing every
	// image.
	if got.Spec.Thresholds.KEVMax == nil || *got.Spec.Thresholds.KEVMax != 0 {
		t.Errorf("thresholds.kevMax = %v, want 0", got.Spec.Thresholds.KEVMax)
	}
	if got.Spec.Thresholds.EPSSMaxPermille == nil || *got.Spec.Thresholds.EPSSMaxPermille != 500 {
		t.Errorf("thresholds.epssMaxPermille = %v, want 500", got.Spec.Thresholds.EPSSMaxPermille)
	}

	// Defaulted() must agree with the API server, or the fake-client tests
	// would be testing a different operator.
	defaulted := previewv1.PreviewCheckSpec{PRNumber: 42, App: "navidrome"}.Defaulted()
	if defaulted.TimeoutSeconds != got.Spec.TimeoutSeconds ||
		defaulted.TTLSeconds != got.Spec.TTLSeconds ||
		defaulted.HTTPPath != got.Spec.HTTPPath ||
		len(defaulted.Checks) != len(got.Spec.Checks) ||
		len(defaulted.ExpectStatus) != len(got.Spec.ExpectStatus) ||
		*defaulted.Thresholds.EPSSMaxPermille != *got.Spec.Thresholds.EPSSMaxPermille {
		t.Errorf("Defaulted() disagrees with the CRD defaults:\n go  = %+v\n crd = %+v", defaulted, got.Spec)
	}
}

func TestEnvtest_ExplicitZeroThresholdSurvivesDefaulting(t *testing.T) {
	cl := requireEnvtest(t)
	ctx := context.Background()

	zero := int32(0)
	pc := envtestCheck(t, func(pc *previewv1.PreviewCheck) {
		pc.Spec.Thresholds.EPSSMaxPermille = &zero
	})
	if err := cl.Create(ctx, pc); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = cl.Delete(ctx, pc) })

	got := &previewv1.PreviewCheck{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: pc.Name}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	// Zero tolerance must be expressible. This is why the leaves are pointers.
	if got.Spec.Thresholds.EPSSMaxPermille == nil || *got.Spec.Thresholds.EPSSMaxPermille != 0 {
		t.Errorf("an explicit epssMaxPermille: 0 was defaulted away: %v", got.Spec.Thresholds.EPSSMaxPermille)
	}
}

func TestEnvtest_PerFieldImmutability(t *testing.T) {
	cl := requireEnvtest(t)
	ctx := context.Background()

	pc := envtestCheck(t, func(pc *previewv1.PreviewCheck) {
		pc.Spec.Image = "ghcr.io/fredericrous/navidrome:0.55.0"
		pc.Spec.Revision = "abc123"
	})
	if err := cl.Create(ctx, pc); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = cl.Delete(ctx, pc) })

	immutable := []struct {
		field  string
		mutate func(*previewv1.PreviewCheck)
	}{
		{"prNumber", func(p *previewv1.PreviewCheck) { p.Spec.PRNumber = 43 }},
		{"app", func(p *previewv1.PreviewCheck) { p.Spec.App = "someone-else" }},
		{"image", func(p *previewv1.PreviewCheck) { p.Spec.Image = "evil:latest" }},
		{"revision", func(p *previewv1.PreviewCheck) { p.Spec.Revision = "deadbeef" }},
		{"httpPath", func(p *previewv1.PreviewCheck) { p.Spec.HTTPPath = "/other" }},
	}
	for _, tc := range immutable {
		t.Run(tc.field, func(t *testing.T) {
			fresh := &previewv1.PreviewCheck{}
			if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: pc.Name}, fresh); err != nil {
				t.Fatalf("get: %v", err)
			}
			tc.mutate(fresh)
			err := cl.Update(ctx, fresh)
			if err == nil {
				t.Fatalf("%s must be immutable: a caller must not be able to re-point a running check", tc.field)
			}
			if !strings.Contains(err.Error(), "immutable") {
				t.Errorf("%s rejection should name immutability, got %v", tc.field, err)
			}
		})
	}

	// The waiting knobs stay mutable on purpose: extending a running check's
	// deadline is a legitimate operator action.
	fresh := &previewv1.PreviewCheck{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: pc.Name}, fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	fresh.Spec.TimeoutSeconds = 3600
	if err := cl.Update(ctx, fresh); err != nil {
		t.Errorf("timeoutSeconds must stay mutable, got %v", err)
	}
}

func TestEnvtest_DuplicateCheckNameRejected(t *testing.T) {
	cl := requireEnvtest(t)
	ctx := context.Background()

	pc := envtestCheck(t)
	if err := cl.Create(ctx, pc); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = cl.Delete(ctx, pc) })

	// status.checks is a listType=map keyed by name so consumers can upsert by
	// name; two entries called "http" would make "did the http check pass?"
	// order-dependent.
	pc.Status.Checks = []previewv1.PreviewCheckResult{
		{Name: previewv1.CheckHTTP, Phase: previewv1.CheckPassed},
		{Name: previewv1.CheckHTTP, Phase: previewv1.CheckFailed},
	}
	if err := cl.Status().Update(ctx, pc); err == nil {
		t.Fatal("duplicate checks[].name must be rejected")
	}
}

func TestEnvtest_StatusRoundTrip(t *testing.T) {
	cl := requireEnvtest(t)
	ctx := context.Background()

	pc := envtestCheck(t)
	if err := cl.Create(ctx, pc); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = cl.Delete(ctx, pc) })

	started := metav1.NewTime(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC))
	pc.Status = previewv1.PreviewCheckStatus{
		Phase:              previewv1.PreviewCheckFailed,
		Message:            "trivy check failed: KEVExceeded",
		PreviewNamespace:   "preview-pr-42",
		PreviewHost:        "pr-42-navidrome.preview.daddyshome.fr",
		ResolvedImage:      "ghcr.io/fredericrous/navidrome:0.55.0",
		StartedAt:          &started,
		CompletedAt:        &started,
		ExpiresAt:          &started,
		ObservedGeneration: 1,
		Checks: []previewv1.PreviewCheckResult{
			{Name: previewv1.CheckReadiness, Phase: previewv1.CheckPassed, StartedAt: &started, FinishedAt: &started},
			{
				Name: previewv1.CheckTrivy, Phase: previewv1.CheckFailed,
				Reason: previewv1.ReasonKEVExceeded, Message: "CVE-2024-0001 is known-exploited",
				Details: map[string]string{"kevCount": "1", "maxEpssPermille": "20"},
			},
			{Name: previewv1.CheckSmoke, Phase: previewv1.CheckSkipped, Reason: previewv1.ReasonNoSmokeTest},
		},
		Conditions: []metav1.Condition{{
			Type: previewv1.ConditionSucceeded, Status: metav1.ConditionFalse,
			Reason: previewv1.ReasonKEVExceeded, Message: "KEVExceeded",
			ObservedGeneration: 1, LastTransitionTime: started,
		}},
	}
	if err := cl.Status().Update(ctx, pc); err != nil {
		t.Fatalf("status update: %v", err)
	}

	got := &previewv1.PreviewCheck{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: pc.Name}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != previewv1.PreviewCheckFailed || got.Status.ResolvedImage == "" {
		t.Errorf("status = %+v", got.Status)
	}
	if len(got.Status.Checks) != 3 {
		t.Fatalf("checks = %+v", got.Status.Checks)
	}
	if got.Status.Checks[1].Details["kevCount"] != "1" {
		t.Errorf("details did not round-trip: %+v", got.Status.Checks[1])
	}
	if len(got.Status.Conditions) != 1 || got.Status.Conditions[0].Reason != previewv1.ReasonKEVExceeded {
		t.Errorf("conditions = %+v", got.Status.Conditions)
	}

	// A status update must not be able to smuggle a spec change through.
	got.Spec.App = "someone-else"
	got.Status.Message = "second write"
	if err := cl.Status().Update(ctx, got); err != nil {
		t.Fatalf("second status update: %v", err)
	}
	after := &previewv1.PreviewCheck{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: pc.Name}, after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Spec.App != "navidrome" {
		t.Errorf("the status subresource wrote the spec: app = %q", after.Spec.App)
	}
}

func TestEnvtest_InvalidSpecRejected(t *testing.T) {
	cl := requireEnvtest(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func(*previewv1.PreviewCheck)
	}{
		{"prNumber below the minimum", func(p *previewv1.PreviewCheck) { p.Spec.PRNumber = 0 }},
		{"unknown check name", func(p *previewv1.PreviewCheck) {
			p.Spec.Checks = []previewv1.PreviewCheckName{"rm-rf"}
		}},
		{"expectStatus outside the HTTP range", func(p *previewv1.PreviewCheck) { p.Spec.ExpectStatus = []int32{99} }},
		{"epssMaxPermille over 1000", func(p *previewv1.PreviewCheck) {
			over := int32(1001)
			p.Spec.Thresholds.EPSSMaxPermille = &over
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := envtestCheck(t)
			pc.Name = fmt.Sprintf("invalid-%d", i)
			tc.mutate(pc)
			if err := cl.Create(ctx, pc); err == nil {
				_ = cl.Delete(ctx, pc)
				t.Fatalf("%s must be rejected by the CRD", tc.name)
			}
		})
	}
}
