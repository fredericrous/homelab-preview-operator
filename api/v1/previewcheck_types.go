package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=prevcheck
// +kubebuilder:printcolumn:name="App",type=string,JSONPath=`.spec.app`
// +kubebuilder:printcolumn:name="PR",type=integer,JSONPath=`.spec.prNumber`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Succeeded")].reason`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PreviewCheck publishes a machine-readable verdict on one preview environment:
// is the change in PR `spec.prNumber` safe to merge?
//
// The CR deliberately lives OUTSIDE the preview namespace (by convention in
// `preview-check`): the preview namespace does not exist yet when the PR opens,
// and it is pruned when the PR closes — a verdict stored inside it would vanish
// with the thing it judged, exactly when the caller needs to read it.
//
// The spec is immutable per scalar field (not as a whole struct): a caller may
// not re-point a running check at another PR, app, image or path, but the CRD
// still accepts the defaulting the API server itself performs.
type PreviewCheck struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PreviewCheckSpec   `json:"spec,omitempty"`
	Status PreviewCheckStatus `json:"status,omitempty"`
}

// PreviewCheckName is one of the checks the operator knows how to run.
// +kubebuilder:validation:Enum=readiness;http;trivy;smoke
type PreviewCheckName string

const (
	// CheckReadiness asserts the preview Kustomization settled and every
	// workload it rendered is available.
	CheckReadiness PreviewCheckName = "readiness"
	// CheckHTTP asserts the app answers with an accepted status from its
	// Service, probed from inside the preview namespace.
	CheckHTTP PreviewCheckName = "http"
	// CheckTrivy asserts the trivy-operator scan of the previewed image carries
	// no exploitable CVE above the thresholds.
	CheckTrivy PreviewCheckName = "trivy"
	// CheckSmoke runs the app's own smoke test, if it declares one.
	CheckSmoke PreviewCheckName = "smoke"
)

// PreviewCheckSpec is the verdict request. Every scalar that selects WHAT is
// judged carries `self == oldSelf`; the knobs that only say HOW long to wait
// stay mutable so an operator can extend a running check.
type PreviewCheckSpec struct {
	// PRNumber is the pull request whose preview is judged. The preview
	// namespace is `preview-pr-<prNumber>`.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="prNumber is immutable"
	PRNumber int32 `json:"prNumber"`

	// App is the previewed application. It is never trusted to SELECT a
	// namespace or a PreviewConfig: the authoritative app name is the preview
	// namespace's `preview-app` label, and a mismatch is a terminal
	// Failed/AppMismatch. Leave it empty to accept whatever the namespace says.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="app is immutable"
	App string `json:"app,omitempty"`

	// Image is the container image under test (`registry/repository:tag`). When
	// empty the operator resolves it from the previewed workload's container.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="image is immutable"
	Image string `json:"image,omitempty"`

	// Revision, when set, gates every check on the preview Kustomization having
	// applied it: `status.lastAppliedRevision` (`sha1:<sha>`) must end with this
	// string. It is how a caller proves the preview runs ITS commit and not the
	// previous push's.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="revision is immutable"
	Revision string `json:"revision,omitempty"`

	// Checks selects which checks run. They always run in the fixed order
	// readiness, http, trivy, smoke regardless of the order listed here, one
	// step per reconcile, fail-fast on the first Failed.
	//
	// Immutable: a check that has already passed is never re-run, so adding one
	// mid-flight would publish a verdict over a set of checks that never all
	// ran together, and removing one would retroactively narrow a verdict the
	// caller may already have read.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:default={readiness,http,trivy,smoke}
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="checks is immutable"
	Checks []PreviewCheckName `json:"checks,omitempty"`

	// HTTPPath is the path the http check requests on the app Service. It is
	// concatenated onto the Service URL, so it must start with "/" — otherwise
	// `health` silently becomes `http://navidrome...:4533health` and the probe
	// fails with a verdict about the app.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^/`
	// +kubebuilder:default="/"
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="httpPath is immutable"
	HTTPPath string `json:"httpPath,omitempty"`

	// ExpectStatus lists the HTTP status codes the http check accepts. The
	// default spans 2xx AND 3xx so an app behind OIDC, which answers the
	// unauthenticated probe with a redirect to the IdP, still passes.
	//
	// Immutable, like httpPath: it is baked into the probe Job's environment at
	// creation and re-read from the spec when the Job's result is interpreted.
	// Editing it mid-flight would score a completed probe against an
	// expectation it never ran under.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Minimum=100
	// +kubebuilder:validation:items:Maximum=599
	// +kubebuilder:default={200,201,202,203,204,301,302,303,307,308}
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="expectStatus is immutable"
	ExpectStatus []int32 `json:"expectStatus,omitempty"`

	// TimeoutSeconds is the run deadline, measured from `status.startedAt`.
	// Reaching it resolves the check: a negative verdict is Failed, an
	// inconclusive one Expired (see PreviewCheckStatus.Phase).
	// +optional
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=21600
	// +kubebuilder:default=1800
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// TTLSeconds is how long the CR itself stays meaningful. Past it a
	// non-terminal check becomes Expired; 24 h later the operator deletes the CR
	// as a backstop (the caller is expected to delete its own CRs).
	// +optional
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=604800
	// +kubebuilder:default=7200
	TTLSeconds int32 `json:"ttlSeconds,omitempty"`

	// Thresholds bounds the trivy check. It is a value struct with a `{}`
	// default on the parent so that the leaf defaults below materialise even
	// when the field is omitted entirely; the leaves are pointers so an explicit
	// `epssMaxPermille: 0` (zero tolerance) stays distinguishable from "unset".
	// +optional
	// +kubebuilder:default={}
	Thresholds PreviewCheckThresholds `json:"thresholds,omitempty"`
}

// PreviewCheckThresholds bounds the exploitability verdict.
//
// Trivy scope, stated rather than implied: the preview is scanned by
// trivy-operator with `severity: CRITICAL,HIGH` and `ignoreUnfixed: true`, so
// these thresholds are evaluated over FIXABLE CRITICAL/HIGH findings only. A
// clean verdict does not mean the image has no vulnerabilities; it means it has
// no fixable critical/high one that is known-exploited or likely to be.
type PreviewCheckThresholds struct {
	// KEVMax is how many findings may appear in CISA's Known Exploited
	// Vulnerabilities catalogue. The default, 0, is "none".
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	KEVMax *int32 `json:"kevMax,omitempty"`

	// EPSSMaxPermille is the highest EPSS score any single finding may carry,
	// expressed in permille (500 == 0.500) so the CRD holds an integer rather
	// than a float. It is an INCLUSIVE maximum: the operator computes
	// `round(epss * 1000)` and fails only when that exceeds this value, so an
	// EPSS of exactly 0.500 passes against the default.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=500
	EPSSMaxPermille *int32 `json:"epssMaxPermille,omitempty"`
}

// Defaulted returns the spec with every CRD default applied in Go.
//
// It exists solely because the fake client used in the unit tests applies no
// CRD defaulting — in a real cluster the API server has already filled all of
// this in before the reconciler ever sees the object. The reconciler calls it
// unconditionally so the two paths cannot drift.
func (s PreviewCheckSpec) Defaulted() PreviewCheckSpec {
	out := *s.DeepCopy()
	if len(out.Checks) == 0 {
		out.Checks = []PreviewCheckName{CheckReadiness, CheckHTTP, CheckTrivy, CheckSmoke}
	}
	if out.HTTPPath == "" {
		out.HTTPPath = "/"
	}
	if len(out.ExpectStatus) == 0 {
		out.ExpectStatus = DefaultExpectStatus()
	}
	if out.TimeoutSeconds <= 0 {
		out.TimeoutSeconds = 1800
	}
	if out.TTLSeconds <= 0 {
		out.TTLSeconds = 7200
	}
	if out.Thresholds.KEVMax == nil {
		out.Thresholds.KEVMax = ptrInt32(0)
	}
	if out.Thresholds.EPSSMaxPermille == nil {
		out.Thresholds.EPSSMaxPermille = ptrInt32(500)
	}
	return out
}

// DefaultExpectStatus is the accepted-status default, kept in one place so the
// CRD marker above and Defaulted cannot disagree.
func DefaultExpectStatus() []int32 {
	return []int32{200, 201, 202, 203, 204, 301, 302, 303, 307, 308}
}

func ptrInt32(v int32) *int32 { return &v }

// PreviewCheckPhase is the lifecycle phase of the whole verdict.
// +kubebuilder:validation:Enum=Pending;Running;Passed;Failed;Expired
type PreviewCheckPhase string

const (
	// PreviewCheckPending means nothing has been judged yet — typically the
	// preview namespace has not rendered, or the Kustomization gate is closed.
	PreviewCheckPending PreviewCheckPhase = "Pending"
	// PreviewCheckRunning means at least one check is in flight.
	PreviewCheckRunning PreviewCheckPhase = "Running"
	// PreviewCheckPassed is terminal: every selected check passed or was skipped.
	PreviewCheckPassed PreviewCheckPhase = "Passed"
	// PreviewCheckFailed is terminal and means the previewed change earned a
	// NEGATIVE verdict — it was judged, and judged bad. The caller closes its PR.
	PreviewCheckFailed PreviewCheckPhase = "Failed"
	// PreviewCheckExpired is terminal and means the change was never judged at
	// all: the scan never appeared, the enrichment endpoint was down, the quota
	// was full, the preview never rendered, or the TTL ran out. The caller
	// leaves its PR open and escalates instead of drawing a conclusion.
	//
	// Expired is reachable at `startedAt + timeoutSeconds`, well before
	// `expiresAt`, so the Expires print column on an Expired object is normally
	// still in the future. That is intentional.
	PreviewCheckExpired PreviewCheckPhase = "Expired"
)

// IsTerminal reports whether a phase is one the reconciler never leaves.
func (p PreviewCheckPhase) IsTerminal() bool {
	switch p {
	case PreviewCheckPassed, PreviewCheckFailed, PreviewCheckExpired:
		return true
	default:
		return false
	}
}

// CheckPhase is the phase of one individual check.
//
// There is deliberately no Expired here: a check that ran out of road is Failed
// with the reason that stopped it, while the CR phase carries the
// judged/not-judged distinction.
// +kubebuilder:validation:Enum=Pending;Running;Passed;Failed;Skipped
type CheckPhase string

const (
	CheckPending CheckPhase = "Pending"
	CheckRunning CheckPhase = "Running"
	CheckPassed  CheckPhase = "Passed"
	CheckFailed  CheckPhase = "Failed"
	CheckSkipped CheckPhase = "Skipped"
)

// IsTerminal reports whether an individual check has finished for good.
func (p CheckPhase) IsTerminal() bool {
	switch p {
	case CheckPassed, CheckFailed, CheckSkipped:
		return true
	default:
		return false
	}
}

// Reasons published on `status.conditions[Succeeded]` and on individual checks.
// They are the caller's contract; treat them as API.
const (
	// ReasonPending is the initial, nothing-observed-yet reason.
	ReasonPending = "Pending"
	// ReasonRunning means checks are in flight.
	ReasonRunning = "Running"
	// ReasonAllChecksPassed is the only passing reason.
	ReasonAllChecksPassed = "AllChecksPassed"

	// ReasonKustomizationNotReady — the preview Kustomization is suspended,
	// un-patched, or not Ready. Failed only when it exists and is explicitly
	// Ready=False at the deadline; otherwise inconclusive.
	ReasonKustomizationNotReady = "KustomizationNotReady"
	// ReasonRevisionMismatch — the preview applied a different commit.
	ReasonRevisionMismatch = "RevisionMismatch"
	// ReasonPodsNotReady — workloads never became available.
	ReasonPodsNotReady = "PodsNotReady"
	// ReasonTargetUnresolved — no HTTPRoute backend or Service to probe.
	ReasonTargetUnresolved = "TargetUnresolved"
	// ReasonUnexpectedStatus — the app answered with a status outside expectStatus.
	ReasonUnexpectedStatus = "UnexpectedStatus"
	// ReasonProbeJobFailed — the probe could not complete a request at all.
	ReasonProbeJobFailed = "ProbeJobFailed"
	// ReasonScanPending — trivy has not reported on this image yet (transient).
	ReasonScanPending = "ScanPending"
	// ReasonScanMissing — trivy never reported before the deadline (inconclusive).
	ReasonScanMissing = "ScanMissing"
	// ReasonKEVExceeded — too many known-exploited findings.
	ReasonKEVExceeded = "KEVExceeded"
	// ReasonEPSSExceeded — a finding's exploit probability is over the threshold.
	ReasonEPSSExceeded = "EPSSExceeded"
	// ReasonEnrichmentUnavailable — KEV/EPSS intelligence could not be trusted.
	// Never read as clean; inconclusive at the deadline.
	ReasonEnrichmentUnavailable = "EnrichmentUnavailable"
	// ReasonInvalidThreshold — the thresholds themselves are unusable.
	ReasonInvalidThreshold = "InvalidThreshold"
	// ReasonNoSmokeTest — the app declares no smoke test; the check is Skipped.
	ReasonNoSmokeTest = "NoSmokeTest"
	// ReasonSmokeJobFailed — the app's own smoke test failed.
	ReasonSmokeJobFailed = "SmokeJobFailed"
	// ReasonAppMismatch — spec.app disagrees with the namespace's preview-app label.
	ReasonAppMismatch = "AppMismatch"
	// ReasonNamespaceGone — the preview namespace is absent or terminating.
	// Failed when it was observed and then vanished, inconclusive when it never
	// rendered at all.
	ReasonNamespaceGone = "NamespaceGone"
	// ReasonQuotaExceeded — the preview ResourceQuota refused the check Job.
	ReasonQuotaExceeded = "QuotaExceeded"
	// ReasonTimeout — the run deadline or the TTL passed with nothing decided.
	ReasonTimeout = "Timeout"
)

// ConditionSucceeded is the single condition type published on a PreviewCheck.
const ConditionSucceeded = "Succeeded"

// PreviewCheckStatus is the published verdict.
type PreviewCheckStatus struct {
	// Phase is the verdict. Passed, Failed and Expired are terminal and never
	// flip back. See PreviewCheckPhase for what Failed vs Expired mean to a caller.
	// +optional
	Phase PreviewCheckPhase `json:"phase,omitempty"`

	// Message is the latest human-readable detail.
	// +optional
	Message string `json:"message,omitempty"`

	// Checks is the per-check detail, upserted by name.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=8
	Checks []PreviewCheckResult `json:"checks,omitempty"`

	// PreviewNamespace is the preview namespace, once it has been OBSERVED.
	// Its emptiness is load-bearing: a namespace that never rendered is
	// inconclusive, one that rendered and then vanished is a failure.
	// +optional
	PreviewNamespace string `json:"previewNamespace,omitempty"`

	// PreviewHost is the preview's external host, for humans. Nothing in the
	// checks uses it: the edge listener demands a client certificate.
	// +optional
	PreviewHost string `json:"previewHost,omitempty"`

	// ResolvedImage is the image the trivy check actually judged.
	// +optional
	ResolvedImage string `json:"resolvedImage,omitempty"`

	// StartedAt is when the operator first reconciled the check; the run
	// deadline is measured from it.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the phase became terminal.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// ExpiresAt is startedAt + ttlSeconds.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions carries the single "Succeeded" condition.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// PreviewCheckResult is one check's outcome.
type PreviewCheckResult struct {
	// Name is the check. It is the list map key, so it is never omitempty.
	Name PreviewCheckName `json:"name"`

	// Phase is this check's phase.
	// +optional
	Phase CheckPhase `json:"phase,omitempty"`

	// Reason is a CamelCase machine-readable reason.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is human-readable detail (log tails are truncated into it).
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when the check first ran.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// FinishedAt is when the check became terminal.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Details carries structured extras (matched image, CVE counts, HTTP status).
	// +optional
	Details map[string]string `json:"details,omitempty"`
}

// +kubebuilder:object:root=true

// PreviewCheckList contains a list of PreviewCheck.
type PreviewCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreviewCheck `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PreviewCheck{}, &PreviewCheckList{})
}
