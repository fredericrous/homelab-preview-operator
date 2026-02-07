package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	v1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

func TestGenerateStripPatches(t *testing.T) {
	patches := generateStripPatches()

	if len(patches) != len(stripTargets) {
		t.Errorf("expected %d strip patches, got %d", len(stripTargets), len(patches))
	}

	// Verify each patch has $patch: delete
	for i, p := range patches {
		if !strings.Contains(p.Patch, "$patch: delete") {
			t.Errorf("patch %d missing $patch: delete directive", i)
		}
		if p.Target == nil {
			t.Errorf("patch %d has nil target", i)
		}
	}

	// Check specific targets are present
	foundNamespace := false
	foundExternalSecret := false
	foundOIDCClient := false
	foundPassword := false
	foundDatabase := false
	for _, p := range patches {
		if p.Target.Kind == "Namespace" && p.Target.Group == "" {
			foundNamespace = true
		}
		if p.Target.Kind == "ExternalSecret" && p.Target.Group == "external-secrets.io" {
			foundExternalSecret = true
		}
		if p.Target.Kind == "OIDCClient" && p.Target.Group == "security.homelab.io" {
			foundOIDCClient = true
		}
		if p.Target.Kind == "Password" && p.Target.Group == "generators.external-secrets.io" {
			foundPassword = true
		}
		if p.Target.Kind == "Database" && p.Target.Group == "postgresql.cnpg.io" {
			foundDatabase = true
		}
	}

	if !foundNamespace {
		t.Error("missing Namespace strip patch")
	}
	if !foundExternalSecret {
		t.Error("missing ExternalSecret strip patch")
	}
	if !foundOIDCClient {
		t.Error("missing OIDCClient strip patch")
	}
	if !foundPassword {
		t.Error("missing Password strip patch")
	}
	if !foundDatabase {
		t.Error("missing Database strip patch")
	}
}

func TestGenerateDeploymentEnvPatches(t *testing.T) {
	envPatches := []EnvPatch{
		{Name: "DATABASE_HOST", Value: "postgres-preview-7-rw.preview-pr-7.svc.cluster.local"},
		{Name: "S3_ENDPOINT", Value: "http://s3proxy.preview-pr-7.svc.cluster.local:8080"},
		{
			Name: "DATABASE_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "postgres-preview-7-superuser"},
					Key:                  "password",
				},
			},
		},
	}

	patch := generateDeploymentEnvPatches("openwebui", "openwebui", envPatches)
	if patch == nil {
		t.Fatal("expected non-nil patch")
	}

	if patch.Target.Kind != "Deployment" {
		t.Errorf("expected Deployment target, got %s", patch.Target.Kind)
	}
	if patch.Target.Name != "openwebui" {
		t.Errorf("expected target name openwebui, got %s", patch.Target.Name)
	}

	// Verify the patch YAML contains our env vars
	if !strings.Contains(patch.Patch, "DATABASE_HOST") {
		t.Error("patch missing DATABASE_HOST")
	}
	if !strings.Contains(patch.Patch, "S3_ENDPOINT") {
		t.Error("patch missing S3_ENDPOINT")
	}
	if !strings.Contains(patch.Patch, "DATABASE_PASSWORD") {
		t.Error("patch missing DATABASE_PASSWORD")
	}
	if !strings.Contains(patch.Patch, "postgres-preview-7-superuser") {
		t.Error("patch missing secretKeyRef name")
	}
}

func TestGenerateDeploymentEnvPatchesEmpty(t *testing.T) {
	patch := generateDeploymentEnvPatches("app", "app", nil)
	if patch != nil {
		t.Error("expected nil patch for empty env patches")
	}
}

func TestGenerateHelmValuePatches(t *testing.T) {
	valuePatches := map[string]string{
		"gitea.config.database.HOST":    "postgres-preview-7-rw.preview-pr-7.svc.cluster.local",
		"gitea.config.database.DB_TYPE": "postgres",
	}

	patches := generateHelmValuePatches("gitea", valuePatches)
	if len(patches) == 0 {
		t.Fatal("expected non-empty patches")
	}

	// Simple paths should produce a strategic merge patch
	found := false
	for _, p := range patches {
		if p.Target.Kind == "HelmRelease" && strings.Contains(p.Patch, "kind: HelmRelease") {
			found = true
		}
	}
	if !found {
		t.Error("missing strategic merge HelmRelease patch")
	}
}

func TestGenerateHelmValuePatchesArrayIndex(t *testing.T) {
	valuePatches := map[string]string{
		"nextcloud.extraEnv[0].value": "redis-preview.preview-pr-8.svc.cluster.local",
		"externalDatabase.host":       "postgres-preview-8-rw.preview-pr-8.svc.cluster.local",
	}

	patches := generateHelmValuePatches("nextcloud", valuePatches)
	if len(patches) < 2 {
		t.Fatalf("expected at least 2 patches (strategic merge + json6902), got %d", len(patches))
	}

	// Should have a JSON6902 patch for array-indexed path
	foundJSON6902 := false
	for _, p := range patches {
		if strings.Contains(p.Patch, "op: replace") && strings.Contains(p.Patch, "/spec/values/nextcloud/extraEnv/0/value") {
			foundJSON6902 = true
		}
	}
	if !foundJSON6902 {
		t.Error("missing JSON6902 patch for array-indexed path")
	}

	// Should have a strategic merge patch for simple path
	foundStrategic := false
	for _, p := range patches {
		if strings.Contains(p.Patch, "kind: HelmRelease") && strings.Contains(p.Patch, "externalDatabase") {
			foundStrategic = true
		}
	}
	if !foundStrategic {
		t.Error("missing strategic merge patch for simple path")
	}
}

func TestGenerateHelmValuePatchesEmpty(t *testing.T) {
	patches := generateHelmValuePatches("app", nil)
	if len(patches) != 0 {
		t.Error("expected empty patches for nil input")
	}
}

func TestDotPathToJSONPointer(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"nextcloud.extraEnv[0].value", "/nextcloud/extraEnv/0/value"},
		{"nextcloud.extraEnv[3].valueFrom.secretKeyRef.name", "/nextcloud/extraEnv/3/valueFrom/secretKeyRef/name"},
		{"simple.path", "/simple/path"},
	}
	for _, tt := range tests {
		got := dotPathToJSONPointer(tt.input)
		if got != tt.expected {
			t.Errorf("dotPathToJSONPointer(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestBuildAllPatches(t *testing.T) {
	config := &v1.PreviewConfig{
		Spec: v1.PreviewConfigSpec{
			DeploymentType: v1.DeploymentTypeDeployment,
			EnvMapping: &v1.EnvMapping{
				DatabaseHost: "DATABASE_HOST",
				S3Endpoint:   "S3_ENDPOINT_URL",
				AppURL:       "WEBUI_URL",
			},
		},
	}

	patches := buildAllPatches("openwebui", "7", "preview-pr-7", "daddyshome.fr", config, "/oauth/oidc/callback")

	// Should have strip patches + deployment env patch
	if len(patches) <= len(stripTargets) {
		t.Errorf("expected more patches than just strip targets, got %d", len(patches))
	}

	// Check that strip patches are present
	stripCount := 0
	for _, p := range patches {
		if strings.Contains(p.Patch, "$patch: delete") {
			stripCount++
		}
	}
	if stripCount != len(stripTargets) {
		t.Errorf("expected %d strip patches, got %d", len(stripTargets), stripCount)
	}

	// Check that deployment env patch is present
	foundDeploymentPatch := false
	for _, p := range patches {
		if p.Target != nil && p.Target.Kind == "Deployment" && p.Target.Name == "openwebui" {
			foundDeploymentPatch = true
			if !strings.Contains(p.Patch, "DATABASE_HOST") {
				t.Error("deployment patch missing DATABASE_HOST")
			}
			if !strings.Contains(p.Patch, "S3_ENDPOINT_URL") {
				t.Error("deployment patch missing S3_ENDPOINT_URL")
			}
		}
	}
	if !foundDeploymentPatch {
		t.Error("missing deployment env patch")
	}
}

func TestBuildAllPatchesHelm(t *testing.T) {
	config := &v1.PreviewConfig{
		Spec: v1.PreviewConfigSpec{
			DeploymentType: v1.DeploymentTypeHelm,
			HelmValues: &v1.HelmValuesMapping{
				DatabaseHost: "gitea.config.database.HOST",
				AppURL:       "gitea.config.server.ROOT_URL",
			},
		},
	}

	patches := buildAllPatches("gitea", "7", "preview-pr-7", "daddyshome.fr", config, "/user/oauth2/Authelia/callback")

	foundHelmPatch := false
	for _, p := range patches {
		if p.Target != nil && p.Target.Kind == "HelmRelease" {
			foundHelmPatch = true
		}
	}
	if !foundHelmPatch {
		t.Error("missing HelmRelease value patch")
	}
}

func TestBuildAllPatchesNoConfig(t *testing.T) {
	config := &v1.PreviewConfig{}

	patches := buildAllPatches("myapp", "1", "preview-pr-1", "daddyshome.fr", config, "/oauth/callback")

	// Should only have strip patches
	if len(patches) != len(stripTargets) {
		t.Errorf("expected only %d strip patches, got %d", len(stripTargets), len(patches))
	}
}

func TestSetNestedMapValue(t *testing.T) {
	m := make(map[string]interface{})
	setNestedMapValue(m, "a.b.c", "hello")

	aMap, ok := m["a"].(map[string]interface{})
	if !ok {
		t.Fatal("a not a map")
	}
	bMap, ok := aMap["b"].(map[string]interface{})
	if !ok {
		t.Fatal("b not a map")
	}
	if bMap["c"] != "hello" {
		t.Errorf("expected hello, got %v", bMap["c"])
	}
}

func TestGenerateRandomString(t *testing.T) {
	s1 := generateRandomString(32)
	s2 := generateRandomString(32)

	if len(s1) != 64 { // 32 bytes = 64 hex chars
		t.Errorf("expected 64 chars, got %d", len(s1))
	}
	if s1 == s2 {
		t.Error("two random strings should not be equal")
	}
}
