// Package v1 contains API types for preview configuration
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pc
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.deploymentType`
// +kubebuilder:printcolumn:name="Redis",type=boolean,JSONPath=`.spec.redis.enabled`
// +kubebuilder:printcolumn:name="Storage",type=boolean,JSONPath=`.spec.storage.enabled`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PreviewConfig defines the preview requirements for an app
type PreviewConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PreviewConfigSpec   `json:"spec,omitempty"`
	Status PreviewConfigStatus `json:"status,omitempty"`
}

// PreviewConfigSpec defines the desired preview configuration
type PreviewConfigSpec struct {
	// DeploymentType specifies how the app is deployed
	// +kubebuilder:validation:Enum=deployment;helm
	// +kubebuilder:default=deployment
	DeploymentType DeploymentType `json:"deploymentType,omitempty"`

	// Redis configuration for preview
	// +optional
	Redis *RedisConfig `json:"redis,omitempty"`

	// Storage configuration for preview (S3/object storage)
	// +optional
	Storage *StorageConfig `json:"storage,omitempty"`

	// EnvMapping defines how to inject preview values into Deployments
	// Used when DeploymentType is "deployment"
	// +optional
	EnvMapping *EnvMapping `json:"envMapping,omitempty"`

	// HelmValues defines paths to values for HelmRelease patching
	// Used when DeploymentType is "helm"
	// +optional
	HelmValues *HelmValuesMapping `json:"helmValues,omitempty"`

	// SharedServices lists services that should use production instances
	// e.g., ["litellm"] means preview uses production litellm
	// +optional
	SharedServices []string `json:"sharedServices,omitempty"`

	// ConfigVolume defines a config PVC to clone with automatic URL rewriting
	// +optional
	ConfigVolume *ConfigVolumeConfig `json:"configVolume,omitempty"`
}

// ConfigVolumeConfig defines a config PVC to clone for preview
type ConfigVolumeConfig struct {
	// ClaimName is the PVC name in the production namespace to snapshot
	ClaimName string `json:"claimName"`
	// SnapshotClass is the VolumeSnapshotClass to use
	// +optional
	// +kubebuilder:default=ceph-block-snapshot
	SnapshotClass string `json:"snapshotClass,omitempty"`
}

// PreviewConfigStatus defines the observed state of PreviewConfig
type PreviewConfigStatus struct {
	// Conditions represent the latest available observations
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastProcessedTime is when the config was last processed
	// +optional
	LastProcessedTime *metav1.Time `json:"lastProcessedTime,omitempty"`
}

// DeploymentType specifies how the app is deployed
// +kubebuilder:validation:Enum=deployment;helm
type DeploymentType string

const (
	DeploymentTypeDeployment DeploymentType = "deployment"
	DeploymentTypeHelm       DeploymentType = "helm"
)

// RedisConfig defines Redis requirements
type RedisConfig struct {
	// Enabled specifies whether the app needs Redis
	Enabled bool `json:"enabled"`
}

// StorageConfig defines object storage requirements
type StorageConfig struct {
	// Enabled specifies whether the app needs S3-compatible storage
	Enabled bool `json:"enabled"`

	// Bucket is the bucket name to use/create
	// +optional
	Bucket string `json:"bucket,omitempty"`
}

// EnvMapping defines environment variable names for Deployment patching
type EnvMapping struct {
	// ContainerNames lists the container names to patch with env overrides.
	// If empty, defaults to the app name.
	// +optional
	ContainerNames []string `json:"containerNames,omitempty"`

	// Database configuration
	// +optional
	DatabaseHost string `json:"databaseHost,omitempty"`
	// +optional
	DatabaseName string `json:"databaseName,omitempty"`
	// +optional
	DatabaseUser string `json:"databaseUser,omitempty"`
	// +optional
	DatabasePassword string `json:"databasePassword,omitempty"`

	// Redis configuration
	// +optional
	RedisHost string `json:"redisHost,omitempty"`
	// +optional
	RedisPort string `json:"redisPort,omitempty"`

	// S3/Storage configuration
	// +optional
	S3Endpoint string `json:"s3Endpoint,omitempty"`
	// +optional
	S3AccessKey string `json:"s3AccessKey,omitempty"`
	// +optional
	S3SecretKey string `json:"s3SecretKey,omitempty"`
	// +optional
	S3Bucket string `json:"s3Bucket,omitempty"`
	// +optional
	S3Region string `json:"s3Region,omitempty"`

	// OIDC configuration
	// +optional
	OIDCClientID string `json:"oidcClientId,omitempty"`
	// +optional
	OIDCClientSecret string `json:"oidcClientSecret,omitempty"`
	// +optional
	OIDCRedirectURI string `json:"oidcRedirectUri,omitempty"`

	// App URL (for OIDC redirect, etc.)
	// +optional
	AppURL string `json:"appUrl,omitempty"`
}

// HelmValuesMapping defines value paths for HelmRelease patching
// Paths use dot notation (e.g., "gitea.config.database.HOST")
type HelmValuesMapping struct {
	// Database configuration
	// +optional
	DatabaseHost string `json:"databaseHost,omitempty"`
	// +optional
	DatabaseName string `json:"databaseName,omitempty"`
	// +optional
	DatabaseUser string `json:"databaseUser,omitempty"`
	// +optional
	DatabasePassword string `json:"databasePassword,omitempty"`

	// Redis configuration
	// +optional
	RedisHost string `json:"redisHost,omitempty"`
	// +optional
	RedisPort string `json:"redisPort,omitempty"`

	// S3/Storage configuration
	// +optional
	S3Endpoint string `json:"s3Endpoint,omitempty"`
	// +optional
	S3AccessKey string `json:"s3AccessKey,omitempty"`
	// +optional
	S3SecretKey string `json:"s3SecretKey,omitempty"`
	// +optional
	S3Bucket string `json:"s3Bucket,omitempty"`

	// OIDC configuration
	// +optional
	OIDCClientID string `json:"oidcClientId,omitempty"`
	// +optional
	OIDCClientSecret string `json:"oidcClientSecret,omitempty"`

	// App URL
	// +optional
	AppURL string `json:"appUrl,omitempty"`
}

// +kubebuilder:object:root=true

// PreviewConfigList contains a list of PreviewConfig
type PreviewConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreviewConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PreviewConfig{}, &PreviewConfigList{})
}
