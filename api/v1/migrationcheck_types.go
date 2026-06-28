package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=migcheck
// +kubebuilder:printcolumn:name="App",type=string,JSONPath=`.spec.appName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MigrationCheck provisions a throwaway, snapshot-based CNPG clone of a
// production database so CI can run an app's migrations against real data
// *before merge*. It is independent of the manual `preview` label flow: the
// clone is created on demand, its connection string is published as a Secret,
// and the whole thing is torn down when the CR is deleted (or its TTL expires).
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
}

// MigrationCheckPhase is the lifecycle phase of a MigrationCheck.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Failed
type MigrationCheckPhase string

const (
	MigrationCheckPending      MigrationCheckPhase = "Pending"
	MigrationCheckProvisioning MigrationCheckPhase = "Provisioning"
	MigrationCheckReady        MigrationCheckPhase = "Ready"
	MigrationCheckFailed       MigrationCheckPhase = "Failed"
)

// MigrationCheckStatus is the observed clone state.
type MigrationCheckStatus struct {
	// Phase is the high-level lifecycle phase. Consumers wait for "Ready".
	// +optional
	Phase MigrationCheckPhase `json:"phase,omitempty"`

	// Message carries the latest human-readable detail (especially on Failed).
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

	// ExpiresAt is when the TTL GC will remove the clone.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
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
