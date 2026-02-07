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
	patches := map[string]string{
		"gitea.config.database.HOST":    "postgres-preview-7-rw.preview-pr-7.svc.cluster.local",
		"gitea.config.database.DB_TYPE": "postgres",
	}

	patch := generateHelmValuePatches("gitea", patches)
	if patch == nil {
		t.Fatal("expected non-nil patch")
	}

	if patch.Target.Kind != "HelmRelease" {
		t.Errorf("expected HelmRelease target, got %s", patch.Target.Kind)
	}
	if patch.Target.Group != "helm.toolkit.fluxcd.io" {
		t.Errorf("expected helm.toolkit.fluxcd.io group, got %s", patch.Target.Group)
	}

	if !strings.Contains(patch.Patch, "kind: HelmRelease") {
		t.Error("patch missing HelmRelease kind")
	}
	if !strings.Contains(patch.Patch, "spec:") {
		t.Error("patch missing spec")
	}
}

func TestGenerateHelmValuePatchesEmpty(t *testing.T) {
	patch := generateHelmValuePatches("app", nil)
	if patch != nil {
		t.Error("expected nil patch for empty value patches")
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
