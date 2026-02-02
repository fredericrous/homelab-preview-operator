// Package v1 contains API types for preview configuration
package v1

// PreviewConfig defines the preview requirements for an app
// This is NOT a CRD - it's the schema for preview.yaml files in app directories
type PreviewConfig struct {
	APIVersion string            `json:"apiVersion" yaml:"apiVersion"`
	Kind       string            `json:"kind" yaml:"kind"`
	Spec       PreviewConfigSpec `json:"spec" yaml:"spec"`
}

// PreviewConfigSpec defines the desired preview configuration
type PreviewConfigSpec struct {
	// DeploymentType specifies how the app is deployed
	// +kubebuilder:validation:Enum=deployment;helm
	// +kubebuilder:default=deployment
	DeploymentType DeploymentType `json:"deploymentType,omitempty" yaml:"deploymentType,omitempty"`

	// Redis configuration for preview
	Redis *RedisConfig `json:"redis,omitempty" yaml:"redis,omitempty"`

	// Storage configuration for preview (S3/object storage)
	Storage *StorageConfig `json:"storage,omitempty" yaml:"storage,omitempty"`

	// EnvMapping defines how to inject preview values into Deployments
	// Used when DeploymentType is "deployment"
	EnvMapping *EnvMapping `json:"envMapping,omitempty" yaml:"envMapping,omitempty"`

	// HelmValues defines paths to values for HelmRelease patching
	// Used when DeploymentType is "helm"
	HelmValues *HelmValuesMapping `json:"helmValues,omitempty" yaml:"helmValues,omitempty"`

	// SharedServices lists services that should use production instances
	// e.g., ["litellm"] means preview uses production litellm
	SharedServices []string `json:"sharedServices,omitempty" yaml:"sharedServices,omitempty"`
}

// DeploymentType specifies how the app is deployed
type DeploymentType string

const (
	DeploymentTypeDeployment DeploymentType = "deployment"
	DeploymentTypeHelm       DeploymentType = "helm"
)

// RedisConfig defines Redis requirements
type RedisConfig struct {
	// Enabled specifies whether the app needs Redis
	Enabled bool `json:"enabled" yaml:"enabled"`
}

// StorageConfig defines object storage requirements
type StorageConfig struct {
	// Enabled specifies whether the app needs S3-compatible storage
	Enabled bool `json:"enabled" yaml:"enabled"`

	// Bucket is the bucket name to use/create
	Bucket string `json:"bucket,omitempty" yaml:"bucket,omitempty"`
}

// EnvMapping defines environment variable names for Deployment patching
type EnvMapping struct {
	// Database configuration
	DatabaseHost     string `json:"databaseHost,omitempty" yaml:"databaseHost,omitempty"`
	DatabaseName     string `json:"databaseName,omitempty" yaml:"databaseName,omitempty"`
	DatabaseUser     string `json:"databaseUser,omitempty" yaml:"databaseUser,omitempty"`
	DatabasePassword string `json:"databasePassword,omitempty" yaml:"databasePassword,omitempty"`

	// Redis configuration
	RedisHost string `json:"redisHost,omitempty" yaml:"redisHost,omitempty"`
	RedisPort string `json:"redisPort,omitempty" yaml:"redisPort,omitempty"`

	// S3/Storage configuration
	S3Endpoint  string `json:"s3Endpoint,omitempty" yaml:"s3Endpoint,omitempty"`
	S3AccessKey string `json:"s3AccessKey,omitempty" yaml:"s3AccessKey,omitempty"`
	S3SecretKey string `json:"s3SecretKey,omitempty" yaml:"s3SecretKey,omitempty"`
	S3Bucket    string `json:"s3Bucket,omitempty" yaml:"s3Bucket,omitempty"`
	S3Region    string `json:"s3Region,omitempty" yaml:"s3Region,omitempty"`

	// OIDC configuration
	OIDCClientID     string `json:"oidcClientId,omitempty" yaml:"oidcClientId,omitempty"`
	OIDCClientSecret string `json:"oidcClientSecret,omitempty" yaml:"oidcClientSecret,omitempty"`
	OIDCRedirectURI  string `json:"oidcRedirectUri,omitempty" yaml:"oidcRedirectUri,omitempty"`

	// App URL (for OIDC redirect, etc.)
	AppURL string `json:"appUrl,omitempty" yaml:"appUrl,omitempty"`
}

// HelmValuesMapping defines value paths for HelmRelease patching
// Paths use dot notation (e.g., "gitea.config.database.HOST")
type HelmValuesMapping struct {
	// Database configuration
	DatabaseHost     string `json:"databaseHost,omitempty" yaml:"databaseHost,omitempty"`
	DatabaseName     string `json:"databaseName,omitempty" yaml:"databaseName,omitempty"`
	DatabaseUser     string `json:"databaseUser,omitempty" yaml:"databaseUser,omitempty"`
	DatabasePassword string `json:"databasePassword,omitempty" yaml:"databasePassword,omitempty"`

	// Redis configuration
	RedisHost string `json:"redisHost,omitempty" yaml:"redisHost,omitempty"`
	RedisPort string `json:"redisPort,omitempty" yaml:"redisPort,omitempty"`

	// S3/Storage configuration
	S3Endpoint  string `json:"s3Endpoint,omitempty" yaml:"s3Endpoint,omitempty"`
	S3AccessKey string `json:"s3AccessKey,omitempty" yaml:"s3AccessKey,omitempty"`
	S3SecretKey string `json:"s3SecretKey,omitempty" yaml:"s3SecretKey,omitempty"`
	S3Bucket    string `json:"s3Bucket,omitempty" yaml:"s3Bucket,omitempty"`

	// OIDC configuration
	OIDCClientID     string `json:"oidcClientId,omitempty" yaml:"oidcClientId,omitempty"`
	OIDCClientSecret string `json:"oidcClientSecret,omitempty" yaml:"oidcClientSecret,omitempty"`

	// App URL
	AppURL string `json:"appUrl,omitempty" yaml:"appUrl,omitempty"`
}
