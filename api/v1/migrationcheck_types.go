package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=migcheck
// +kubebuilder:printcolumn:name="App",type=string,JSONPath=`.spec.appName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiresAt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MigrationCheck provisions a throwaway, snapshot-based CNPG clone of a
// production database so an app's migrations can be run against real data
// *before merge*. It is independent of the manual `preview` label flow: the
// clone is created on demand, its connection string is published as a Secret,
// and the whole thing is torn down when the CR is deleted (or its TTL expires).
//
// Two ways to consume it:
//
//   - Push (legacy): a CI runner creates the CR, waits for Ready, reads the
//     connection Secret and runs the app itself. Nothing else happens.
//   - Pull: `spec.probe` names the app image under test and the operator runs
//     it against the clone in a Job, turning the CR into a verdict
//     (Running -> Passed/Failed/Expired). With `spec.report` the verdict is also
//     published as a GitHub check run on the PR's head commit.
//
// The restore brings the entire source CNPG cluster; the published DATABASE_URL
// targets the `spec.appName` database within it.
type MigrationCheck struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MigrationCheckSpec   `json:"spec,omitempty"`
	Status MigrationCheckStatus `json:"status,omitempty"`
}

// MigrationCheckSpec is the desired clone.
type MigrationCheckSpec struct {
	// AppName is the production app whose database is cloned. By default the
	// source CNPG cluster is discovered from the production namespace's
	// `postgres.cnpg.io/cluster-name` annotation, and the published DATABASE_URL
	// targets the database named after the app.
	AppName string `json:"appName"`

	// ClusterName overrides the source CNPG cluster name (defaults to the
	// production namespace's `postgres.cnpg.io/cluster-name` annotation).
	// +optional
	ClusterName string `json:"clusterName,omitempty"`

	// ClusterNamespace overrides the source CNPG cluster namespace (defaults to
	// the `postgres.cnpg.io/cluster-namespace` annotation, else "postgres").
	// +optional
	ClusterNamespace string `json:"clusterNamespace,omitempty"`

	// DatabaseName overrides the database the DATABASE_URL targets (defaults to
	// AppName, matching the homelab CNPG Database CR convention).
	// +optional
	DatabaseName string `json:"databaseName,omitempty"`

	// TTLSeconds is how long the clone may live before the operator GCs it even
	// if the CR is never deleted (defends against abandoned CI jobs).
	// +optional
	// +kubebuilder:default=3600
	TTLSeconds int64 `json:"ttlSeconds,omitempty"`

	// ReadyTimeoutSeconds bounds the clone bring-up when a probe is configured.
	// A clone that has not settled by then makes the check Expired (never
	// judged) instead of holding a PR's check in-progress until the TTL. It has
	// no effect without `probe`: the push flow polls Ready with its own budget.
	// +optional
	// +kubebuilder:default=1500
	// +kubebuilder:validation:Minimum=60
	ReadyTimeoutSeconds int64 `json:"readyTimeoutSeconds,omitempty"`

	// Probe, when set, runs the app under test against the clone once it is
	// Ready and turns this MigrationCheck into a verdict. Nil keeps the push
	// behaviour: publish the connection Secret and wait.
	// +optional
	Probe *MigrationProbe `json:"probe,omitempty"`

	// Report, when set, publishes the verdict as a check run on the forge.
	// Credentials are never selected here; the operator uses the GitHub App
	// named by its own --github-app-secret flag.
	// +optional
	Report *MigrationReport `json:"report,omitempty"`
}

// MigrationProbe describes how to run the app under test against the clone.
//
// The app image runs with its own entrypoint as a sidecar and gets the clone's
// DATABASE_URL injected by the operator (from the connection Secret, never from
// this spec); the operator's probe container polls the readiness path on
// localhost and the Job's exit code is the verdict.
// +kubebuilder:validation:XValidation:rule="!has(self.env) || !('DATABASE_URL' in self.env)",message="DATABASE_URL is injected by the operator and must not be set in probe.env"
type MigrationProbe struct {
	// Image is the app image under test, e.g. ghcr.io/owner/app:pr-<sha>.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Image string `json:"image"`

	// Port is the localhost port the app listens on.
	// +optional
	// +kubebuilder:default=3000
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// ReadyPath is the HTTP path polled until it answers 2xx.
	// +optional
	// +kubebuilder:default="/"
	// +kubebuilder:validation:Pattern=`^/`
	// +kubebuilder:validation:MaxLength=512
	ReadyPath string `json:"readyPath,omitempty"`

	// Expect is a substring the response body must contain for the poll to
	// count as ready. Empty means any 2xx answer is enough.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Expect string `json:"expect,omitempty"`

	// RunAsUser is the numeric uid the app container runs as. Required when the
	// image sets its USER by name: the pod runs with runAsNonRoot, and the
	// kubelet refuses to start a container whose image user it cannot prove is
	// non-root ("image has non-numeric user"). Must match the image's user for
	// files the app owns.
	// +optional
	// +kubebuilder:validation:Minimum=1
	RunAsUser *int64 `json:"runAsUser,omitempty"`

	// Env is extra plain-value environment for the app container. There is no
	// valueFrom on purpose: a CR must not be able to name a Secret.
	// +optional
	// +kubebuilder:validation:MaxProperties=32
	Env map[string]string `json:"env,omitempty"`

	// TimeoutSeconds bounds the poll: the time the app gets to boot, run its
	// migrations and answer ReadyPath.
	// +optional
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=3600
	TimeoutSeconds int64 `json:"timeoutSeconds,omitempty"`
}

// MigrationReport says where the verdict is published.
type MigrationReport struct {
	// Provider is the forge flavour. Only github (check runs) is supported.
	// +optional
	// +kubebuilder:default=github
	// +kubebuilder:validation:Enum=github
	Provider string `json:"provider,omitempty"`

	// Repo is "owner/name".
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`
	Repo string `json:"repo"`

	// Revision is the commit the check run attaches to (the PR head sha).
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{7,40}$`
	Revision string `json:"revision"`

	// Name is the check run name shown on the PR.
	// +optional
	// +kubebuilder:default="migration-check"
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name,omitempty"`
}

// MigrationCheckPhase is the lifecycle phase of a MigrationCheck.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Running;Passed;Failed;Expired
type MigrationCheckPhase string

const (
	MigrationCheckPending      MigrationCheckPhase = "Pending"
	MigrationCheckProvisioning MigrationCheckPhase = "Provisioning"
	// MigrationCheckReady means the clone accepts connections and the
	// connection Secret is published. Without a probe this is where the CR
	// parks until deleted or TTL.
	MigrationCheckReady MigrationCheckPhase = "Ready"
	// MigrationCheckRunning means the probe Job is in flight.
	MigrationCheckRunning MigrationCheckPhase = "Running"
	// MigrationCheckPassed is terminal: the app booted against the clone and
	// answered its readiness path.
	MigrationCheckPassed MigrationCheckPhase = "Passed"
	// MigrationCheckFailed is terminal and means the change was judged BAD: the
	// probe ran and the app did not come up. (Without a probe it also carries
	// provisioning errors, for the push flow's benefit.)
	MigrationCheckFailed MigrationCheckPhase = "Failed"
	// MigrationCheckExpired is terminal and means the change was never judged:
	// the clone never settled, the image never pulled, or the probe ran out of
	// time. A caller escalates instead of drawing a conclusion.
	MigrationCheckExpired MigrationCheckPhase = "Expired"
)

// IsTerminal reports whether a phase is one the reconciler never leaves.
func (p MigrationCheckPhase) IsTerminal() bool {
	switch p {
	case MigrationCheckPassed, MigrationCheckFailed, MigrationCheckExpired:
		return true
	default:
		return false
	}
}

// Reasons published on `status.reason`. They are the caller's contract.
const (
	MigrationReasonProvisioning     = "Provisioning"
	MigrationReasonReady            = "Ready"
	MigrationReasonCloneFailed      = "CloneFailed"
	MigrationReasonCloneNotReady    = "CloneNotReady"
	MigrationReasonProbeRunning     = "ProbeRunning"
	MigrationReasonProbePassed      = "ProbePassed"
	MigrationReasonProbeFailed      = "ProbeFailed"
	MigrationReasonImageUnavailable = "ImageUnavailable"
	// MigrationReasonPodNotStarted: the kubelet refused to start a container
	// (CreateContainerConfigError, e.g. a non-numeric image user under
	// runAsNonRoot). A configuration miss, never a verdict on the migrations.
	MigrationReasonPodNotStarted    = "PodNotStarted"
	MigrationReasonDeadlineExceeded = "DeadlineExceeded"
	MigrationReasonTTLTooShort      = "TTLTooShort"
)

// MigrationCheckStatus is the observed clone state.
type MigrationCheckStatus struct {
	// Phase is the high-level lifecycle phase. Push consumers wait for "Ready";
	// the pull flow ends in Passed, Failed or Expired.
	// +optional
	Phase MigrationCheckPhase `json:"phase,omitempty"`

	// Reason is the machine-readable cause of the current phase.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message carries the latest human-readable detail (especially on Failed),
	// including the probe log tail once the probe has finished.
	// +optional
	Message string `json:"message,omitempty"`

	// ConnectionSecretName is the Secret holding DATABASE_URL for the clone.
	// It lives in this CR's namespace so the (minimally-privileged) CI runner can
	// read it. Only set once Phase == Ready (i.e. the clone actually accepts
	// connections).
	// +optional
	ConnectionSecretName string `json:"connectionSecretName,omitempty"`

	// ConnectionSecretNamespace is where that Secret lives (this CR's namespace).
	// Tracked explicitly so teardown can delete it even though it is outside the
	// throwaway clone namespace.
	// +optional
	ConnectionSecretNamespace string `json:"connectionSecretNamespace,omitempty"`

	// CloneNamespace is the throwaway namespace holding the CNPG clone.
	// +optional
	CloneNamespace string `json:"cloneNamespace,omitempty"`

	// CloneImage is the CNPG operand image the clone runs. The probe Job's
	// wait-db init container runs pg_isready from it, so the gate always
	// matches the server it is waiting for and pulls nothing new.
	// +optional
	CloneImage string `json:"cloneImage,omitempty"`

	// ExpiresAt is when the TTL GC will remove the clone.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// StartedAt is when provisioning began; the ready deadline counts from it.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// ProbeJobName is the probe Job in this CR's namespace, once created.
	// +optional
	ProbeJobName string `json:"probeJobName,omitempty"`

	// ProbeStartedAt is when the probe Job was created; the probe deadline
	// counts from it.
	// +optional
	ProbeStartedAt *metav1.Time `json:"probeStartedAt,omitempty"`

	// CompletedAt is when the CR reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// CheckRunID is the forge check run created for this CR, so later updates
	// patch it instead of creating another.
	// +optional
	CheckRunID int64 `json:"checkRunID,omitempty"`

	// ReportedPhase is the last phase successfully published to the forge.
	// +optional
	ReportedPhase MigrationCheckPhase `json:"reportedPhase,omitempty"`
}

// +kubebuilder:object:root=true

// MigrationCheckList contains a list of MigrationCheck.
type MigrationCheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MigrationCheck `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MigrationCheck{}, &MigrationCheckList{})
}
