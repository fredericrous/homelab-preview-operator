// Package previewcheck holds the pure decision functions behind the
// PreviewCheck verdict: given data already fetched from the cluster, is this
// preview good?
//
// Nothing here imports controller-runtime, and nothing here performs I/O. That
// is the point: the interesting judgements (does this scan describe the image
// under test, is an EPSS of exactly 0.500 acceptable, does replicas=0 count as
// ready) are the parts most worth testing, and they are testable as plain
// values rather than through a fake API server.
package previewcheck

import (
	"fmt"
	"math"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
)

// Artifact is the image a trivy VulnerabilityReport describes, read out of
// `report.artifact`.
type Artifact struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

// String renders the artifact the way a pod spec would.
func (a Artifact) String() string {
	ref := a.Repository
	if a.Registry != "" {
		ref = a.Registry + "/" + a.Repository
	}
	switch {
	case a.Tag != "":
		return ref + ":" + a.Tag
	case a.Digest != "":
		return ref + "@" + a.Digest
	default:
		return ref
	}
}

// ImageRef is a parsed container image reference.
type ImageRef struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

// ParseImage splits `[registry/]repository[:tag][@digest]`.
//
// The registry is only recognised as such when the first path segment looks
// like a host (contains a dot or a colon, or is exactly "localhost") — the
// same rule the container tooling uses, and the reason `library/nginx:1` parses
// as a repository rather than a registry called "library".
func ParseImage(image string) ImageRef {
	var ref ImageRef
	rest := strings.TrimSpace(image)

	if at := strings.LastIndex(rest, "@"); at >= 0 {
		ref.Digest = rest[at+1:]
		rest = rest[:at]
	}
	if colon := strings.LastIndex(rest, ":"); colon >= 0 && !strings.Contains(rest[colon+1:], "/") {
		ref.Tag = rest[colon+1:]
		rest = rest[:colon]
	}
	if slash := strings.Index(rest, "/"); slash >= 0 {
		head := rest[:slash]
		if head == "localhost" || strings.ContainsAny(head, ".:") {
			ref.Registry = head
			rest = rest[slash+1:]
		}
	}
	ref.Repository = rest
	return ref
}

// MatchesImage reports whether a scan artifact describes the image under test.
//
// The comparison is deliberately asymmetric about the registry: trivy-operator
// records the fully-qualified artifact (`ghcr.io/owner/app`) while a pod spec —
// and therefore the caller's `spec.image` — frequently omits the default
// registry. So a want without a registry matches any registry, and a want WITH
// one must match exactly; that way a preview pulling from the local zot mirror
// is never confused with the same repository on ghcr.io when the caller cared
// enough to say which.
//
// A digest-pinned want is matched on the digest alone: the tag of a digest pin
// is decoration, and homelab's `tag@sha256:` pins have historically carried a
// stale tag next to the authoritative digest.
func MatchesImage(a Artifact, want string) bool {
	if want == "" {
		return false
	}
	w := ParseImage(want)

	if w.Digest != "" {
		return a.Digest != "" && strings.EqualFold(a.Digest, w.Digest)
	}
	if w.Repository == "" || !strings.EqualFold(a.Repository, w.Repository) {
		return false
	}
	if w.Registry != "" && !strings.EqualFold(a.Registry, w.Registry) {
		return false
	}
	if w.Tag != "" {
		return a.Tag == w.Tag
	}
	return true
}

// ExtractCVEs keeps only real CVE ids, de-duplicated and sorted, and returns
// everything it dropped.
//
// The KEV/EPSS intelligence is keyed on CVE ids only. Trivy also reports GHSA-,
// DSA-, ALAS- and vendor-specific ids, which the enrichment endpoint has never
// heard of; posting them would inflate the "unknown" set and, worse, make an
// image look enriched when nothing about it was actually looked up. The dropped
// ids are handed back so they can be published as detail rather than silently
// swallowed.
func ExtractCVEs(ids []string) (cves, dropped []string) {
	seenCVE := map[string]bool{}
	seenOther := map[string]bool{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(id), "CVE-") {
			if !seenCVE[id] {
				seenCVE[id] = true
				cves = append(cves, id)
			}
			continue
		}
		if !seenOther[id] {
			seenOther[id] = true
			dropped = append(dropped, id)
		}
	}
	sort.Strings(cves)
	sort.Strings(dropped)
	return cves, dropped
}

// CVEIntel is one CVE's exploitability, as the enrichment endpoint reports it.
type CVEIntel struct {
	CVE  string  `json:"cve"`
	KEV  bool    `json:"kev"`
	EPSS float64 `json:"epss"`
}

// Exploitability is the verdict over a set of enriched CVEs.
type Exploitability struct {
	// Passed is the verdict.
	Passed bool
	// Reason is empty when Passed, else KEVExceeded or EPSSExceeded.
	Reason string
	// KEVCount is how many findings are in the KEV catalogue.
	KEVCount int
	// KEVIDs lists them (bounded by the caller's CVE set).
	KEVIDs []string
	// MaxPermille is the highest round(epss*1000) seen.
	MaxPermille int32
	// MaxCVE is the id that carried MaxPermille.
	MaxCVE string
	// Detail is a human-readable summary.
	Detail string
}

// Reason strings returned by EvaluateExploitability. They mirror the API
// reasons in api/v1 but are kept as plain strings so this package stays free of
// any dependency on the API types.
const (
	ReasonKEVExceeded  = "KEVExceeded"
	ReasonEPSSExceeded = "EPSSExceeded"
)

// EvaluateExploitability decides whether a set of enriched CVEs is within the
// thresholds.
//
// epssMaxPermille is an INCLUSIVE maximum over `round(epss * 1000)`. The
// rounding and the inclusivity are both load-bearing: a float threshold
// comparison on a score the source publishes to three decimals turns "EPSS
// 0.5" into a coin flip, and an exclusive bound would reject exactly the score
// an operator typed as the limit. So 0.4995 rounds to 500 and passes a 500
// threshold, 0.5004 rounds to 500 and passes, and 0.501 rounds to 501 and
// fails. (0.5005 also passes: its nearest float64 is 0.50049999999999994 — the
// reason the CRD holds an integer permille rather than a float in the first
// place.)
//
// KEV is checked first: being on the known-exploited list is a stronger signal
// than any probability, and the reason a caller sees should name it.
func EvaluateExploitability(results []CVEIntel, kevMax, epssMaxPermille int32) Exploitability {
	var out Exploitability
	for _, r := range results {
		if r.KEV {
			out.KEVCount++
			out.KEVIDs = append(out.KEVIDs, r.CVE)
		}
		if p := Permille(r.EPSS); p > out.MaxPermille {
			out.MaxPermille = p
			out.MaxCVE = r.CVE
		}
	}
	sort.Strings(out.KEVIDs)

	switch {
	case out.KEVCount > int(kevMax):
		out.Reason = ReasonKEVExceeded
		out.Detail = fmt.Sprintf("%d known-exploited CVE(s) (max %d): %s",
			out.KEVCount, kevMax, strings.Join(out.KEVIDs, ", "))
	case out.MaxPermille > epssMaxPermille:
		out.Reason = ReasonEPSSExceeded
		out.Detail = fmt.Sprintf("%s has EPSS %.3f (max %.3f)",
			out.MaxCVE, float64(out.MaxPermille)/1000, float64(epssMaxPermille)/1000)
	default:
		out.Passed = true
		out.Detail = fmt.Sprintf("%d CVE(s) enriched, %d known-exploited, peak EPSS %.3f",
			len(results), out.KEVCount, float64(out.MaxPermille)/1000)
	}
	return out
}

// Permille converts an EPSS probability to the integer permille the CRD holds.
func Permille(epss float64) int32 {
	if epss <= 0 || math.IsNaN(epss) {
		return 0
	}
	if epss >= 1 {
		return 1000
	}
	return int32(math.Round(epss * 1000))
}

// StatusAccepted reports whether an HTTP status code is in the accepted set.
// An empty set accepts nothing: "no expectation" must never read as "anything
// goes" on a check whose whole job is to have an expectation.
func StatusAccepted(status int, accepted []int32) bool {
	for _, a := range accepted {
		if int(a) == status {
			return true
		}
	}
	return false
}

// JobPhase is a Job's outcome as the reconciler cares about it.
type JobPhase string

const (
	// JobRunning means the Job has neither succeeded nor failed yet.
	JobRunning JobPhase = "Running"
	// JobSucceeded means at least one pod exited 0.
	JobSucceeded JobPhase = "Succeeded"
	// JobFailed means the Job gave up (backoff exhausted, deadline exceeded).
	JobFailed JobPhase = "Failed"
)

// JobOutcome reduces a Job to Running/Succeeded/Failed plus the reason and
// message the Job itself published.
//
// Conditions are consulted before the counters because a Job killed by
// activeDeadlineSeconds reports `Failed/DeadlineExceeded` as a condition — the
// one failure mode a caller most needs told apart from "the probe ran and the
// app answered wrong".
func JobOutcome(job *batchv1.Job) (phase JobPhase, reason, message string) {
	if job == nil {
		return JobRunning, "", ""
	}
	for _, c := range job.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return JobSucceeded, c.Reason, c.Message
		case batchv1.JobFailed:
			return JobFailed, c.Reason, c.Message
		}
	}
	if job.Status.Succeeded > 0 {
		return JobSucceeded, "", ""
	}
	if job.Status.Failed > 0 && job.Spec.BackoffLimit != nil && job.Status.Failed > *job.Spec.BackoffLimit {
		return JobFailed, "BackoffLimitExceeded", "job pods failed"
	}
	return JobRunning, "", ""
}

// EvaluateWorkloadReadiness reports whether every previewed workload is
// available.
//
// `replicas: 0` counts as ready. A preview legitimately scales things to zero
// (a worker with no queue, a cronjob-only Deployment), and reading "no ready
// replicas" as "not ready" would hang every such preview until the deadline
// and then call a perfectly good change bad.
func EvaluateWorkloadReadiness(deployments []appsv1.Deployment, statefulsets []appsv1.StatefulSet) (ready bool, detail string) {
	var pending []string
	total := 0

	for i := range deployments {
		d := &deployments[i]
		total++
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		if want == 0 {
			continue
		}
		if d.Status.ReadyReplicas < want || d.Status.ObservedGeneration < d.Generation {
			pending = append(pending, fmt.Sprintf("deployment/%s %d/%d", d.Name, d.Status.ReadyReplicas, want))
		}
	}
	for i := range statefulsets {
		s := &statefulsets[i]
		total++
		want := int32(1)
		if s.Spec.Replicas != nil {
			want = *s.Spec.Replicas
		}
		if want == 0 {
			continue
		}
		if s.Status.ReadyReplicas < want || s.Status.ObservedGeneration < s.Generation {
			pending = append(pending, fmt.Sprintf("statefulset/%s %d/%d", s.Name, s.Status.ReadyReplicas, want))
		}
	}

	if total == 0 {
		// Nothing rendered yet. The Kustomization gate should have caught this,
		// but an empty namespace is not evidence of health either way.
		return false, "no workloads found"
	}
	if len(pending) > 0 {
		sort.Strings(pending)
		return false, strings.Join(pending, ", ")
	}
	return true, fmt.Sprintf("%d workload(s) available", total)
}

// CheckID returns a stable, DNS-safe 12-character id derived from a CR UID.
//
// It is derived from the UID, never the PR number: a reopened or force-pushed
// PR mints a new CR, and reusing the PR number would collide its check Jobs
// with the previous run's, which may still be terminating.
func CheckID(uid, fallbackName string) string {
	id := strings.ReplaceAll(uid, "-", "")
	if len(id) > 12 {
		id = id[:12]
	}
	if id == "" {
		id = "n" + fallbackName
	}
	return strings.ToLower(id)
}

// TruncateLogTail caps a log tail at maxBytes, keeping the END (where the
// failure is) and prefixing an ellipsis when it cut.
func TruncateLogTail(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := s[len(s)-maxBytes:]
	if nl := strings.IndexByte(cut, '\n'); nl >= 0 && nl < len(cut)-1 {
		cut = cut[nl+1:]
	}
	return "...\n" + cut
}
