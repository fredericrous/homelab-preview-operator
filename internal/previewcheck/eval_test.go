package previewcheck

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseImage(t *testing.T) {
	cases := []struct {
		in   string
		want ImageRef
	}{
		{"nginx", ImageRef{Repository: "nginx"}},
		{"nginx:1.27", ImageRef{Repository: "nginx", Tag: "1.27"}},
		{"library/nginx:1.27", ImageRef{Repository: "library/nginx", Tag: "1.27"}},
		{"ghcr.io/owner/app:v1", ImageRef{Registry: "ghcr.io", Repository: "owner/app", Tag: "v1"}},
		{"localhost:5000/app:v1", ImageRef{Registry: "localhost:5000", Repository: "app", Tag: "v1"}},
		{"ghcr.io/owner/app@sha256:abc", ImageRef{Registry: "ghcr.io", Repository: "owner/app", Digest: "sha256:abc"}},
		// The homelab pin shape: a tag AND a digest on the same ref.
		{"ghcr.io/owner/app:v1@sha256:abc", ImageRef{Registry: "ghcr.io", Repository: "owner/app", Tag: "v1", Digest: "sha256:abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := ParseImage(tc.in); got != tc.want {
				t.Errorf("ParseImage(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestMatchesImage(t *testing.T) {
	ghcr := Artifact{Registry: "ghcr.io", Repository: "fredericrous/navidrome", Tag: "0.55.0", Digest: "sha256:aaa"}
	cases := []struct {
		name string
		art  Artifact
		want string
		ok   bool
	}{
		{"fully qualified", ghcr, "ghcr.io/fredericrous/navidrome:0.55.0", true},
		{"registry omitted matches any registry", ghcr, "fredericrous/navidrome:0.55.0", true},
		{"wrong registry when one is given", ghcr, "docker.io/fredericrous/navidrome:0.55.0", false},
		{"wrong tag", ghcr, "fredericrous/navidrome:0.54.0", false},
		{"wrong repository", ghcr, "fredericrous/other:0.55.0", false},
		{"no tag matches the repository", ghcr, "fredericrous/navidrome", true},
		{"digest pin matches on digest", ghcr, "fredericrous/navidrome@sha256:aaa", true},
		// A stale tag next to an authoritative digest is exactly the homelab
		// pin shape that repo_scan mis-parsed; the digest must win.
		{"digest pin ignores a stale tag", ghcr, "fredericrous/navidrome:0.1.0@sha256:aaa", true},
		{"digest pin with a different digest", ghcr, "fredericrous/navidrome@sha256:bbb", false},
		{"empty want never matches", ghcr, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchesImage(tc.art, tc.want); got != tc.ok {
				t.Errorf("MatchesImage(%s, %q) = %v, want %v", tc.art, tc.want, got, tc.ok)
			}
		})
	}
}

func TestExtractCVEs_DropsNonCVEIDs(t *testing.T) {
	cves, dropped := ExtractCVEs([]string{
		"CVE-2024-1234", "GHSA-xxxx-yyyy-zzzz", "CVE-2024-0001", "CVE-2024-1234",
		"DSA-5555-1", "ALAS2-2024-1", "", "  ",
	})
	wantCVEs := []string{"CVE-2024-0001", "CVE-2024-1234"}
	if strings.Join(cves, ",") != strings.Join(wantCVEs, ",") {
		t.Errorf("cves = %v, want %v (sorted and de-duplicated)", cves, wantCVEs)
	}
	wantDropped := []string{"ALAS2-2024-1", "DSA-5555-1", "GHSA-xxxx-yyyy-zzzz"}
	if strings.Join(dropped, ",") != strings.Join(wantDropped, ",") {
		t.Errorf("dropped = %v, want %v", dropped, wantDropped)
	}
}

func TestPermilleRounding(t *testing.T) {
	cases := []struct {
		epss float64
		want int32
	}{
		{0, 0},
		{-1, 0},
		{0.4994, 499},
		{0.4995, 500}, // rounds UP to exactly the default threshold, and passes
		{0.5004, 500},
		// 0.5005 is NOT here on purpose: the nearest float64 to it is
		// 0.50049999999999994, so it rounds to 500. That is the honest answer
		// for a score the source publishes to three decimals — and the reason
		// the CRD holds an integer permille rather than a float at all.
		{0.5005, 500},
		{0.501, 501}, // the first score that actually fails a 500 threshold
		{1, 1000},
		{2, 1000},
	}
	for _, tc := range cases {
		if got := Permille(tc.epss); got != tc.want {
			t.Errorf("Permille(%v) = %d, want %d", tc.epss, got, tc.want)
		}
	}
}

func TestEvaluateExploitability_EPSSBoundaryIsInclusive(t *testing.T) {
	cases := []struct {
		name       string
		permille   float64
		wantPassed bool
		wantReason string
	}{
		{"499 passes", 0.499, true, ""},
		{"500 passes — the maximum is INCLUSIVE", 0.500, true, ""},
		{"501 fails", 0.501, false, ReasonEPSSExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateExploitability([]CVEIntel{{CVE: "CVE-1", EPSS: tc.permille}}, 0, 500)
			if got.Passed != tc.wantPassed || got.Reason != tc.wantReason {
				t.Errorf("EvaluateExploitability(epss=%v) = passed %v reason %q, want %v %q",
					tc.permille, got.Passed, got.Reason, tc.wantPassed, tc.wantReason)
			}
		})
	}
}

func TestEvaluateExploitability_KEVBeatsEPSS(t *testing.T) {
	results := []CVEIntel{
		{CVE: "CVE-2", KEV: true, EPSS: 0.01},
		{CVE: "CVE-1", EPSS: 0.99},
	}
	got := EvaluateExploitability(results, 0, 500)
	if got.Passed {
		t.Fatal("a known-exploited CVE must not pass")
	}
	// Both thresholds are blown; the reason must name the stronger signal.
	if got.Reason != ReasonKEVExceeded {
		t.Errorf("reason = %q, want %q", got.Reason, ReasonKEVExceeded)
	}
	if got.KEVCount != 1 || got.KEVIDs[0] != "CVE-2" {
		t.Errorf("kev = %d %v", got.KEVCount, got.KEVIDs)
	}
	if got.MaxPermille != 990 || got.MaxCVE != "CVE-1" {
		t.Errorf("peak = %d (%s), want 990 (CVE-1)", got.MaxPermille, got.MaxCVE)
	}
}

func TestEvaluateExploitability_RaisedThresholds(t *testing.T) {
	results := []CVEIntel{{CVE: "CVE-1", KEV: true, EPSS: 0.7}}
	if got := EvaluateExploitability(results, 1, 700); !got.Passed {
		t.Errorf("one KEV under a kevMax of 1 and EPSS at exactly the max must pass: %+v", got)
	}
	if got := EvaluateExploitability(nil, 0, 500); !got.Passed {
		t.Errorf("an empty result set passes: %+v", got)
	}
}

func TestStatusAccepted(t *testing.T) {
	defaults := []int32{200, 201, 202, 203, 204, 301, 302, 303, 307, 308}
	if !StatusAccepted(302, defaults) {
		t.Error("302 must pass the default set: an OIDC-fronted app redirects an unauthenticated probe")
	}
	if StatusAccepted(302, []int32{200}) {
		t.Error("302 must fail an explicit [200]")
	}
	if StatusAccepted(200, nil) {
		t.Error("an empty accepted set must accept nothing, never everything")
	}
	if !StatusAccepted(200, []int32{200}) {
		t.Error("200 must pass [200]")
	}
}

func TestJobOutcome(t *testing.T) {
	backoff := int32(0)
	running := &batchv1.Job{Spec: batchv1.JobSpec{BackoffLimit: &backoff}}
	if phase, _, _ := JobOutcome(running); phase != JobRunning {
		t.Errorf("fresh job = %s, want Running", phase)
	}
	if phase, _, _ := JobOutcome(nil); phase != JobRunning {
		t.Errorf("nil job = %s, want Running", phase)
	}

	done := &batchv1.Job{Status: batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
		Succeeded:  1,
	}}
	if phase, _, _ := JobOutcome(done); phase != JobSucceeded {
		t.Errorf("complete job = %s, want Succeeded", phase)
	}

	// A Job killed by activeDeadlineSeconds must be distinguishable from one
	// whose container reported a bad status.
	deadline := &batchv1.Job{Status: batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "DeadlineExceeded", Message: "Job was active longer than specified deadline",
		}},
	}}
	phase, reason, msg := JobOutcome(deadline)
	if phase != JobFailed || reason != "DeadlineExceeded" || msg == "" {
		t.Errorf("deadline job = %s/%s/%q, want Failed/DeadlineExceeded with a message", phase, reason, msg)
	}

	// A False condition must not be read as the condition holding.
	notYet := &batchv1.Job{Status: batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionFalse}},
	}}
	if phase, _, _ := JobOutcome(notYet); phase != JobRunning {
		t.Errorf("Failed=False job = %s, want Running", phase)
	}
}

func TestEvaluateWorkloadReadiness(t *testing.T) {
	dep := func(name string, replicas, ready int32, gen, observed int64) appsv1.Deployment {
		return appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Generation: gen},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: ready, ObservedGeneration: observed},
		}
	}

	if ready, detail := EvaluateWorkloadReadiness(nil, nil); ready {
		t.Errorf("an empty namespace is not evidence of health: %s", detail)
	}

	ready, detail := EvaluateWorkloadReadiness([]appsv1.Deployment{dep("app", 1, 1, 2, 2)}, nil)
	if !ready {
		t.Errorf("1/1 deployment must be ready: %s", detail)
	}

	// replicas=0 counts as ready: a preview legitimately scales a worker to
	// zero, and calling that "not ready" would hang every such preview.
	ready, detail = EvaluateWorkloadReadiness([]appsv1.Deployment{
		dep("app", 1, 1, 1, 1), dep("worker", 0, 0, 1, 1),
	}, nil)
	if !ready {
		t.Errorf("replicas=0 must count as ready: %s", detail)
	}

	ready, detail = EvaluateWorkloadReadiness([]appsv1.Deployment{dep("app", 2, 1, 1, 1)}, nil)
	if ready || !strings.Contains(detail, "deployment/app 1/2") {
		t.Errorf("1/2 deployment must not be ready, detail = %q", detail)
	}

	// A generation the controller has not observed means the status describes
	// the PREVIOUS spec: "ready" there is last rollout's answer.
	ready, _ = EvaluateWorkloadReadiness([]appsv1.Deployment{dep("app", 1, 1, 3, 2)}, nil)
	if ready {
		t.Error("an unobserved generation must not count as ready")
	}

	replicas := int32(1)
	sts := appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 0, ObservedGeneration: 1},
	}
	if ready, _ := EvaluateWorkloadReadiness(nil, []appsv1.StatefulSet{sts}); ready {
		t.Error("a 0/1 statefulset must not be ready")
	}
}

func TestCheckID(t *testing.T) {
	a := CheckID("3f2504e0-4f89-41d3-9a0c-0305e82c3301", "pr-7")
	if a != CheckID("3f2504e0-4f89-41d3-9a0c-0305e82c3301", "pr-7") {
		t.Error("CheckID must be stable")
	}
	if len(a) != 12 {
		t.Errorf("CheckID len = %d, want 12", len(a))
	}
	if strings.ContainsAny(a, "-_.") {
		t.Errorf("CheckID must be DNS-safe, got %q", a)
	}
	// Distinct UIDs must not collide: a reopened PR reuses the PR number but
	// never the UID, and colliding Job names would fight a terminating Job.
	if CheckID("00000000-1111-2222-3333-444444444444", "pr-7") == a {
		t.Error("distinct UIDs produced the same id")
	}
	if got := CheckID("", "pr-7"); got != "npr-7" {
		t.Errorf("fallback = %q, want npr-7", got)
	}
}

func TestTruncateLogTail(t *testing.T) {
	if got := TruncateLogTail("short", 100); got != "short" {
		t.Errorf("under the cap must pass through, got %q", got)
	}
	long := strings.Repeat("a\n", 200)
	got := TruncateLogTail(long, 50)
	if len(got) > 54 {
		t.Errorf("tail = %d bytes, want <= 54", len(got))
	}
	if !strings.HasPrefix(got, "...") {
		t.Errorf("a cut tail must say so, got %q", got)
	}
	// The END is what matters: a failure prints last.
	if !strings.HasSuffix(got, "a\n") {
		t.Errorf("tail must keep the end of the log, got %q", got)
	}
}
