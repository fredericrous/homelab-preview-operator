package controller

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	previewv1 "github.com/fredericrous/homelab-preview-operator/api/v1"
	"github.com/fredericrous/homelab-preview-operator/internal/enrichment"
	"github.com/fredericrous/homelab-preview-operator/internal/previewcheck"
)

// checkStep is one check's outcome for one reconcile pass.
type checkStep struct {
	phase   previewv1.CheckPhase
	reason  string
	message string
	details map[string]string

	// requeue overrides the default ladder while the check is Running.
	requeue time.Duration

	// crPhase overrides the reason-to-phase table for a Failed step. It is only
	// set where the same reason can mean either "judged bad" or "never judged" —
	// a 4xx from the enrichment endpoint (our request is wrong: Failed) versus
	// the endpoint being down (Expired).
	crPhase previewv1.PreviewCheckPhase
}

var (
	gvkHTTPRoute = schema.GroupVersionKind{
		Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute",
	}
	gvkVulnReportList = schema.GroupVersionKind{
		Group: "aquasecurity.github.io", Version: "v1alpha1", Kind: "VulnerabilityReportList",
	}

	// probeStatusRe reads the status code the probe container printed.
	probeStatusRe = regexp.MustCompile(`status=(\d{3})`)
)

// evaluateCheck runs one step of the named check.
func (r *PreviewCheckReconciler) evaluateCheck(ctx context.Context, log logr.Logger, pc *previewv1.PreviewCheck, spec previewv1.PreviewCheckSpec, env *checkEnv, name previewv1.PreviewCheckName) (checkStep, error) {
	switch name {
	case previewv1.CheckReadiness:
		return r.checkReadiness(ctx, env)
	case previewv1.CheckHTTP:
		return r.checkHTTP(ctx, log, pc, spec, env)
	case previewv1.CheckTrivy:
		return r.checkTrivy(ctx, log, pc, spec, env)
	case previewv1.CheckSmoke:
		return r.checkSmoke(ctx, log, pc, env)
	default:
		// The CRD enum makes this unreachable from the API server, but a check
		// name the operator does not implement must never read as a pass.
		return checkStep{
			phase:   previewv1.CheckFailed,
			reason:  previewv1.ReasonInvalidThreshold,
			message: fmt.Sprintf("unknown check %q", name),
		}, nil
	}
}

// checkReadiness asserts every workload the preview rendered is available.
//
// The Kustomization gate upstream has already proven Flux applied the right
// revision, so this check is purely about the pods.
func (r *PreviewCheckReconciler) checkReadiness(ctx context.Context, env *checkEnv) (checkStep, error) {
	deployments := &appsv1.DeploymentList{}
	if err := r.List(ctx, deployments, client.InNamespace(env.nsName)); err != nil {
		if step, ok := namespaceGoneStep(err); ok {
			return step, nil
		}
		return checkStep{}, fmt.Errorf("list deployments in %s: %w", env.nsName, err)
	}
	statefulsets := &appsv1.StatefulSetList{}
	if err := r.List(ctx, statefulsets, client.InNamespace(env.nsName)); err != nil {
		if step, ok := namespaceGoneStep(err); ok {
			return step, nil
		}
		return checkStep{}, fmt.Errorf("list statefulsets in %s: %w", env.nsName, err)
	}

	ready, detail := previewcheck.EvaluateWorkloadReadiness(deployments.Items, statefulsets.Items)
	if !ready {
		return checkStep{phase: previewv1.CheckRunning, reason: previewv1.ReasonPodsNotReady, message: detail}, nil
	}
	return checkStep{phase: previewv1.CheckPassed, message: detail}, nil
}

// checkHTTP probes the app's Service from inside the preview namespace.
//
// It proves the app serves a status code from its Service — not that the edge
// path (gateway, TLS, HTTPRoute) works. The edge listener demands a client
// certificate, so probing it from a Job would test our certificate handling
// rather than the change. App-level assertions belong in the smoke test.
func (r *PreviewCheckReconciler) checkHTTP(ctx context.Context, log logr.Logger, pc *previewv1.PreviewCheck, spec previewv1.PreviewCheckSpec, env *checkEnv) (checkStep, error) {
	target, err := r.resolveHTTPTarget(ctx, log, env)
	if err != nil {
		return checkStep{}, err
	}
	if target.service == "" {
		return checkStep{
			phase:   previewv1.CheckRunning,
			reason:  previewv1.ReasonTargetUnresolved,
			message: fmt.Sprintf("no HTTPRoute backend or app Service found in %s", env.nsName),
		}, nil
	}

	url := serviceURL(target, env.nsName) + spec.HTTPPath
	job, step, err := r.ensureJob(ctx, env, r.probeJob(env, pc, spec, url))
	if err != nil || step != nil {
		return deref(step), err
	}

	phase, jobReason, jobMessage := previewcheck.JobOutcome(job)
	details := map[string]string{"targetUrl": url}
	switch phase {
	case previewcheck.JobSucceeded:
		return checkStep{phase: previewv1.CheckPassed, message: "app answered with an accepted status", details: details}, nil
	case previewcheck.JobRunning:
		return checkStep{phase: previewv1.CheckRunning, message: "probe job running", details: details, requeue: 5 * time.Second}, nil
	}

	tail := r.tailJob(ctx, log, env.nsName, job.Name)
	reason := previewv1.ReasonProbeJobFailed
	if code, ok := parseProbeStatus(tail); ok {
		details["httpStatus"] = strconv.Itoa(code)
		if !previewcheck.StatusAccepted(code, spec.ExpectStatus) {
			reason = previewv1.ReasonUnexpectedStatus
		}
	}
	return checkStep{
		phase:   previewv1.CheckFailed,
		reason:  reason,
		message: joinDetail(fmt.Sprintf("probe job failed (%s %s)", jobReason, jobMessage), tail),
		details: details,
	}, nil
}

// checkTrivy re-scores the previewed image's CVEs against the thresholds.
func (r *PreviewCheckReconciler) checkTrivy(ctx context.Context, log logr.Logger, pc *previewv1.PreviewCheck, spec previewv1.PreviewCheckSpec, env *checkEnv) (checkStep, error) {
	image := spec.Image
	if image == "" {
		resolved, err := r.resolveWorkloadImage(ctx, env)
		if err != nil {
			return checkStep{}, err
		}
		image = resolved
	}
	if image == "" {
		return checkStep{
			phase:   previewv1.CheckRunning,
			reason:  previewv1.ReasonTargetUnresolved,
			message: fmt.Sprintf("could not resolve the previewed image in %s", env.nsName),
		}, nil
	}
	pc.Status.ResolvedImage = image

	reports := &unstructured.UnstructuredList{}
	reports.SetGroupVersionKind(gvkVulnReportList)
	if err := r.List(ctx, reports, client.InNamespace(env.nsName)); err != nil {
		if step, ok := namespaceGoneStep(err); ok {
			return step, nil
		}
		return checkStep{}, fmt.Errorf("list vulnerabilityreports in %s: %w", env.nsName, err)
	}

	var ids []string
	matched := ""
	for i := range reports.Items {
		artifact := reportArtifact(&reports.Items[i])
		if !previewcheck.MatchesImage(artifact, image) {
			continue
		}
		matched = artifact.String()
		ids = append(ids, reportVulnerabilityIDs(&reports.Items[i])...)
	}
	if matched == "" {
		// trivy-operator queues roughly 240 workloads ahead of a new preview, so
		// "not scanned yet" is the normal state for several minutes.
		return checkStep{
			phase:   previewv1.CheckRunning,
			reason:  previewv1.ReasonScanPending,
			message: fmt.Sprintf("no VulnerabilityReport for %s yet", image),
			requeue: 30 * time.Second,
		}, nil
	}

	cves, dropped := previewcheck.ExtractCVEs(ids)
	details := map[string]string{
		"image":    matched,
		"findings": strconv.Itoa(len(ids)),
		"cves":     strconv.Itoa(len(cves)),
	}
	if len(dropped) > 0 {
		details["nonCveIds"] = truncateMessage(strings.Join(dropped, ","), 512)
	}

	if len(cves) == 0 {
		// The enricher is NOT called: an empty list earns a 400, which the
		// ladder below reads as terminal — a clean image would fail closed.
		return checkStep{
			phase:   previewv1.CheckPassed,
			message: fmt.Sprintf("%s: no fixable CRITICAL/HIGH CVE findings", matched),
			details: details,
		}, nil
	}

	if r.Enricher == nil {
		return checkStep{
			phase:   previewv1.CheckFailed,
			reason:  previewv1.ReasonEnrichmentUnavailable,
			crPhase: previewv1.PreviewCheckExpired,
			message: "no CVE enrichment endpoint is configured; refusing to read an unenriched scan as clean",
			details: details,
		}, nil
	}

	resp, err := r.Enricher.Lookup(ctx, cves)
	switch {
	case err != nil && enrichment.IsTerminal(err):
		// A 4xx will be a 4xx again in thirty seconds: retrying is pointless.
		return checkStep{
			phase:   previewv1.CheckFailed,
			reason:  previewv1.ReasonEnrichmentUnavailable,
			crPhase: previewv1.PreviewCheckFailed,
			message: fmt.Sprintf("CVE enrichment rejected the request: %v", err),
			details: details,
		}, nil
	case err != nil:
		log.V(1).Info("cve enrichment unavailable, will retry", "error", err.Error())
		return checkStep{
			phase:   previewv1.CheckRunning,
			reason:  previewv1.ReasonEnrichmentUnavailable,
			message: fmt.Sprintf("CVE enrichment unavailable: %v", err),
			details: details,
			requeue: 30 * time.Second,
		}, nil
	case !resp.Usable():
		// A well-formed 200 over an empty or stale cache says "I do not know",
		// and every CVE would come back unknown — i.e. clean. Never read that as
		// a pass.
		return checkStep{
			phase:  previewv1.CheckRunning,
			reason: previewv1.ReasonEnrichmentUnavailable,
			message: fmt.Sprintf("CVE enrichment is not usable (stale=%t kevTotal=%d, fetched %s)",
				resp.Stale, resp.KEVTotal, resp.FetchedAt.Format(time.RFC3339)),
			details: details,
			requeue: 30 * time.Second,
		}, nil
	}

	thresholds := spec.Thresholds
	if thresholds.KEVMax == nil || thresholds.EPSSMaxPermille == nil {
		return checkStep{
			phase:   previewv1.CheckFailed,
			reason:  previewv1.ReasonInvalidThreshold,
			message: "thresholds are not fully defaulted",
			details: details,
		}, nil
	}

	details["unknownCves"] = strconv.Itoa(len(resp.Unknown))
	verdict := previewcheck.EvaluateExploitability(resp.Results, *thresholds.KEVMax, *thresholds.EPSSMaxPermille)
	details["kevCount"] = strconv.Itoa(verdict.KEVCount)
	details["maxEpssPermille"] = strconv.Itoa(int(verdict.MaxPermille))
	if !verdict.Passed {
		return checkStep{
			phase:   previewv1.CheckFailed,
			reason:  verdict.Reason,
			message: fmt.Sprintf("%s: %s", matched, verdict.Detail),
			details: details,
		}, nil
	}
	return checkStep{
		phase:   previewv1.CheckPassed,
		message: fmt.Sprintf("%s: %s", matched, verdict.Detail),
		details: details,
	}, nil
}

// checkSmoke runs the app's own test, if it declares one.
//
// The PreviewConfig is read from the PRODUCTION namespace — key {app, app},
// with `app` taken from the preview namespace's validated label, never from
// spec.app. A pull request therefore cannot introduce or edit the test that
// judges it.
func (r *PreviewCheckReconciler) checkSmoke(ctx context.Context, log logr.Logger, pc *previewv1.PreviewCheck, env *checkEnv) (checkStep, error) {
	cfg := &previewv1.PreviewConfig{}
	err := r.Get(ctx, types.NamespacedName{Namespace: env.app, Name: env.app}, cfg)
	switch {
	case apierrors.IsNotFound(err):
		return checkStep{
			phase:   previewv1.CheckSkipped,
			reason:  previewv1.ReasonNoSmokeTest,
			message: fmt.Sprintf("no PreviewConfig %s/%s", env.app, env.app),
		}, nil
	case err != nil:
		return checkStep{}, fmt.Errorf("get previewconfig %s/%s: %w", env.app, env.app, err)
	case cfg.Spec.SmokeTest == nil || cfg.Spec.SmokeTest.Image == "":
		return checkStep{
			phase:   previewv1.CheckSkipped,
			reason:  previewv1.ReasonNoSmokeTest,
			message: fmt.Sprintf("PreviewConfig %s/%s declares no smokeTest", env.app, env.app),
		}, nil
	}

	target, err := r.resolveHTTPTarget(ctx, log, env)
	if err != nil {
		return checkStep{}, err
	}
	previewURL := ""
	if target.service != "" {
		previewURL = serviceURL(target, env.nsName)
	}

	job, step, err := r.ensureJob(ctx, env, r.smokeJob(env, pc, cfg.Spec.SmokeTest, previewURL))
	if err != nil || step != nil {
		return deref(step), err
	}

	phase, jobReason, jobMessage := previewcheck.JobOutcome(job)
	details := map[string]string{"image": cfg.Spec.SmokeTest.Image}
	switch phase {
	case previewcheck.JobSucceeded:
		return checkStep{phase: previewv1.CheckPassed, message: "smoke test passed", details: details}, nil
	case previewcheck.JobRunning:
		return checkStep{phase: previewv1.CheckRunning, message: "smoke job running", details: details, requeue: 10 * time.Second}, nil
	}

	tail := r.tailJob(ctx, log, env.nsName, job.Name)
	return checkStep{
		phase:   previewv1.CheckFailed,
		reason:  previewv1.ReasonSmokeJobFailed,
		message: joinDetail(fmt.Sprintf("smoke job failed (%s %s)", jobReason, jobMessage), tail),
		details: details,
	}, nil
}

// httpTarget is the Service the http check probes.
type httpTarget struct {
	service string
	port    int32
}

func serviceURL(t httpTarget, namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", t.service, namespace, t.port)
}

// resolveHTTPTarget finds what to probe, preferring what the edge already
// points at.
//
// The HTTPRoute's first backendRef is authoritative because it is literally the
// Service a human visiting the preview would reach. Only when there is no route
// — an app previewed without an ingress, or a route that has not rendered yet —
// does it fall back to a Service named after the app, and then to the single
// non-infrastructure Service in the namespace. The infrastructure filter
// matters: a preview namespace also holds the CNPG clone's -rw/-ro/-r Services,
// a Redis and an S3 proxy, and probing Postgres over HTTP would fail in a way
// that reads as an app failure.
func (r *PreviewCheckReconciler) resolveHTTPTarget(ctx context.Context, log logr.Logger, env *checkEnv) (httpTarget, error) {
	var target httpTarget

	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(gvkHTTPRoute)
	routeName := fmt.Sprintf("preview-%d", env.prNumber)
	if err := r.Get(ctx, types.NamespacedName{Namespace: env.nsName, Name: routeName}, route); err != nil {
		// A missing route, or a cluster without the Gateway API installed, is a
		// reason to fall back — not a reason to fail somebody's pull request.
		log.V(1).Info("no preview HTTPRoute, falling back to Services", "route", routeName, "error", err.Error())
	} else if name, port, ok := firstBackendRef(route); ok {
		target = httpTarget{service: name, port: port}
	}

	services := &corev1.ServiceList{}
	if err := r.List(ctx, services, client.InNamespace(env.nsName)); err != nil {
		if namespaceUnavailable(err) {
			return httpTarget{}, nil
		}
		return httpTarget{}, fmt.Errorf("list services in %s: %w", env.nsName, err)
	}

	byName := map[string]*corev1.Service{}
	var candidates []*corev1.Service
	for i := range services.Items {
		svc := &services.Items[i]
		byName[svc.Name] = svc
		if !isInfraService(svc.Name) {
			candidates = append(candidates, svc)
		}
	}

	if target.service == "" {
		switch {
		case byName[env.app] != nil:
			target.service = env.app
		case len(candidates) == 1:
			target.service = candidates[0].Name
		default:
			return httpTarget{}, nil
		}
	}
	if target.port == 0 {
		svc := byName[target.service]
		if svc == nil || len(svc.Spec.Ports) == 0 {
			return httpTarget{}, nil
		}
		target.port = svc.Spec.Ports[0].Port
	}
	return target, nil
}

// firstBackendRef reads spec.rules[0].backendRefs[0] off an HTTPRoute.
func firstBackendRef(route *unstructured.Unstructured) (name string, port int32, ok bool) {
	rules, found, err := unstructured.NestedSlice(route.Object, "spec", "rules")
	if err != nil || !found || len(rules) == 0 {
		return "", 0, false
	}
	rule, isMap := rules[0].(map[string]any)
	if !isMap {
		return "", 0, false
	}
	refs, found, err := unstructured.NestedSlice(rule, "backendRefs")
	if err != nil || !found || len(refs) == 0 {
		return "", 0, false
	}
	ref, isMap := refs[0].(map[string]any)
	if !isMap {
		return "", 0, false
	}
	name, _, _ = unstructured.NestedString(ref, "name")
	p, _, _ := unstructured.NestedInt64(ref, "port")
	if name == "" {
		return "", 0, false
	}
	// An unstructured field is whatever the API server stored; a port outside
	// the legal range is reported as absent so the Service lookup supplies one.
	if p < 1 || p > 65535 {
		return name, 0, true
	}
	return name, int32(p), true
}

// isInfraService filters out the Services a preview namespace holds that are
// not the app: the CNPG clone's read/write endpoints, Redis, the S3 proxy.
func isInfraService(name string) bool {
	switch {
	case strings.HasPrefix(name, "redis"), strings.Contains(name, "s3proxy"):
		return true
	case strings.HasSuffix(name, "-rw"), strings.HasSuffix(name, "-ro"),
		strings.HasSuffix(name, "-r"), strings.HasSuffix(name, "-any"):
		return true
	default:
		return false
	}
}

// resolveWorkloadImage picks the image the trivy check should judge when the
// caller did not name one: the app's own container, preferring a workload and a
// container named after the app.
func (r *PreviewCheckReconciler) resolveWorkloadImage(ctx context.Context, env *checkEnv) (string, error) {
	deployments := &appsv1.DeploymentList{}
	if err := r.List(ctx, deployments, client.InNamespace(env.nsName)); err != nil {
		if namespaceUnavailable(err) {
			return "", nil
		}
		return "", fmt.Errorf("list deployments in %s: %w", env.nsName, err)
	}
	names := make([]string, 0, len(deployments.Items))
	pods := map[string]corev1.PodSpec{}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		names = append(names, d.Name)
		pods[d.Name] = d.Spec.Template.Spec
	}

	statefulsets := &appsv1.StatefulSetList{}
	if err := r.List(ctx, statefulsets, client.InNamespace(env.nsName)); err != nil {
		if namespaceUnavailable(err) {
			return "", nil
		}
		return "", fmt.Errorf("list statefulsets in %s: %w", env.nsName, err)
	}
	for i := range statefulsets.Items {
		s := &statefulsets.Items[i]
		if _, dup := pods[s.Name]; !dup {
			names = append(names, s.Name)
		}
		pods[s.Name] = s.Spec.Template.Spec
	}

	sort.Strings(names)
	if spec, found := pods[env.app]; found {
		if img := containerImage(spec, env.app); img != "" {
			return img, nil
		}
	}
	for _, n := range names {
		if isInfraService(n) {
			continue
		}
		if img := containerImage(pods[n], env.app); img != "" {
			return img, nil
		}
	}
	return "", nil
}

func containerImage(spec corev1.PodSpec, app string) string {
	for _, c := range spec.Containers {
		if c.Name == app {
			return c.Image
		}
	}
	if len(spec.Containers) > 0 {
		return spec.Containers[0].Image
	}
	return ""
}

// reportArtifact reads the image a trivy VulnerabilityReport describes.
func reportArtifact(report *unstructured.Unstructured) previewcheck.Artifact {
	registry, _, _ := unstructured.NestedString(report.Object, "report", "registry", "server")
	repository, _, _ := unstructured.NestedString(report.Object, "report", "artifact", "repository")
	tag, _, _ := unstructured.NestedString(report.Object, "report", "artifact", "tag")
	digest, _, _ := unstructured.NestedString(report.Object, "report", "artifact", "digest")
	return previewcheck.Artifact{Registry: registry, Repository: repository, Tag: tag, Digest: digest}
}

// reportVulnerabilityIDs reads every finding id out of a VulnerabilityReport.
func reportVulnerabilityIDs(report *unstructured.Unstructured) []string {
	items, found, err := unstructured.NestedSlice(report.Object, "report", "vulnerabilities")
	if err != nil || !found {
		return nil
	}
	var ids []string
	for _, raw := range items {
		v, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, _, _ := unstructured.NestedString(v, "vulnerabilityID"); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// namespaceGoneStep turns a "the namespace is going away" API error into the
// terminal verdict for a namespace we had already observed.
func namespaceGoneStep(err error) (checkStep, bool) {
	if !namespaceUnavailable(err) {
		return checkStep{}, false
	}
	return checkStep{
		phase:   previewv1.CheckFailed,
		reason:  previewv1.ReasonNamespaceGone,
		crPhase: previewv1.PreviewCheckFailed,
		message: "the preview namespace went away while the check was running",
	}, true
}

// tailJob fetches a failed Job's log tail. Best-effort by design: a missing log
// is missing DETAIL, never a different verdict.
func (r *PreviewCheckReconciler) tailJob(ctx context.Context, log logr.Logger, namespace, name string) string {
	if r.Logs == nil {
		return ""
	}
	tail, err := r.Logs.TailJobLogs(ctx, namespace, name, logTailBytes)
	if err != nil {
		log.V(1).Info("could not tail check job logs", "job", name, "error", err.Error())
		return ""
	}
	return previewcheck.TruncateLogTail(strings.TrimSpace(tail), logTailBytes)
}

// parseProbeStatus reads the status code the probe container printed, which is
// what separates "the app answered 500" (UnexpectedStatus — a verdict about the
// change) from "the probe never got an answer" (ProbeJobFailed).
func parseProbeStatus(tail string) (int, bool) {
	m := probeStatusRe.FindAllStringSubmatch(tail, -1)
	if len(m) == 0 {
		return 0, false
	}
	code, err := strconv.Atoi(m[len(m)-1][1])
	if err != nil {
		return 0, false
	}
	return code, true
}

func joinDetail(head, tail string) string {
	if tail == "" {
		return head
	}
	return head + "\n" + tail
}

func deref(s *checkStep) checkStep {
	if s == nil {
		return checkStep{}
	}
	return *s
}
