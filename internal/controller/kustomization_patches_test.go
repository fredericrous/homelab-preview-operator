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

	patch := generateDeploymentEnvPatches("openwebui", []string{"openwebui"}, envPatches)
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
	patch := generateDeploymentEnvPatches("app", nil, nil)
	if patch != nil {
		t.Error("expected nil patch for empty env patches")
	}
}

func TestGenerateHelmValuePatches(t *testing.T) {
	valuePatches := map[string]string{
		"gitea.config.database.HOST":    "postgres-preview-7-rw.preview-pr-7.svc.cluster.local",
		"gitea.config.database.DB_TYPE": "postgres",
	}

	patch := generateHelmValuePatches("gitea", valuePatches)
	if patch == nil {
		t.Fatal("expected non-nil patch")
	}

	if patch.Target.Kind != "HelmRelease" {
		t.Errorf("expected HelmRelease target, got %s", patch.Target.Kind)
	}
	if !strings.Contains(patch.Patch, "kind: HelmRelease") {
		t.Error("missing HelmRelease kind in patch")
	}
	if !strings.Contains(patch.Patch, "HOST") {
		t.Error("missing HOST in patch")
	}
}

func TestGenerateHelmValuePatchesSkipsArrayPaths(t *testing.T) {
	valuePatches := map[string]string{
		"nextcloud.extraEnv[0].value": "should-be-skipped",
		"externalDatabase.host":       "postgres-preview-8-rw.preview-pr-8.svc.cluster.local",
	}

	patch := generateHelmValuePatches("nextcloud", valuePatches)
	if patch == nil {
		t.Fatal("expected non-nil patch for simple path")
	}

	// Array-indexed path should be skipped
	if strings.Contains(patch.Patch, "extraEnv") {
		t.Error("array-indexed path should be skipped")
	}
	// Simple path should be present
	if !strings.Contains(patch.Patch, "externalDatabase") {
		t.Error("simple path should be present")
	}
}

func TestGenerateHelmValuePatchesOnlyArrayPaths(t *testing.T) {
	valuePatches := map[string]string{
		"nextcloud.extraEnv[0].value": "should-be-skipped",
	}

	patch := generateHelmValuePatches("nextcloud", valuePatches)
	if patch != nil {
		t.Error("expected nil when all paths are array-indexed")
	}
}

func TestGenerateHelmValuePatchesEmpty(t *testing.T) {
	patch := generateHelmValuePatches("app", nil)
	if patch != nil {
		t.Error("expected nil patch for nil input")
	}
}

func TestGeneratePostRendererPatch(t *testing.T) {
	envPatches := []EnvPatch{
		{Name: "REDIS_HOST", Value: "redis-preview.preview-pr-8.svc.cluster.local"},
		{
			Name: "OIDC_CLIENT_SECRET",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "preview-oidc-client-secret-8"},
					Key:                  "client_secret",
				},
			},
		},
	}

	patch := generatePostRendererPatch("nextcloud", []string{"nextcloud"}, envPatches)
	if patch == nil {
		t.Fatal("expected non-nil patch")
	}

	if patch.Target.Kind != "HelmRelease" {
		t.Errorf("expected HelmRelease target, got %s", patch.Target.Kind)
	}
	if patch.Target.Name != "nextcloud" {
		t.Errorf("expected target name nextcloud, got %s", patch.Target.Name)
	}

	// Should contain postRenderers
	if !strings.Contains(patch.Patch, "postRenderers") {
		t.Error("patch missing postRenderers")
	}

	// Should contain inner Deployment patch
	if !strings.Contains(patch.Patch, "kind: Deployment") {
		t.Error("inner patch missing Deployment kind")
	}

	// Should contain env vars by name
	if !strings.Contains(patch.Patch, "REDIS_HOST") {
		t.Error("patch missing REDIS_HOST")
	}
	if !strings.Contains(patch.Patch, "OIDC_CLIENT_SECRET") {
		t.Error("patch missing OIDC_CLIENT_SECRET")
	}
	if !strings.Contains(patch.Patch, "preview-oidc-client-secret-8") {
		t.Error("patch missing secretKeyRef name")
	}

	// Should be a valid HelmRelease patch
	if !strings.Contains(patch.Patch, "kind: HelmRelease") {
		t.Error("outer patch missing HelmRelease kind")
	}
}

func TestGeneratePostRendererPatchEmpty(t *testing.T) {
	patch := generatePostRendererPatch("app", nil, nil)
	if patch != nil {
		t.Error("expected nil for empty env patches")
	}
}

func TestGeneratePostRendererPatchMultiContainer(t *testing.T) {
	envPatches := []EnvPatch{
		{Name: "REDIS_HOST", Value: "redis-preview.preview-pr-8.svc.cluster.local"},
		{
			Name: "OIDC_CLIENT_SECRET",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "preview-oidc-client-secret-8"},
					Key:                  "client_secret",
				},
			},
		},
	}

	patch := generatePostRendererPatch("nextcloud", []string{"nextcloud", "nextcloud-cron"}, envPatches)
	if patch == nil {
		t.Fatal("expected non-nil patch")
	}

	// Both containers should be present
	if !strings.Contains(patch.Patch, "name: nextcloud\n") {
		t.Error("patch missing nextcloud container")
	}
	if !strings.Contains(patch.Patch, "name: nextcloud-cron") {
		t.Error("patch missing nextcloud-cron container")
	}

	// Both containers should have env vars
	// Count occurrences of REDIS_HOST — should be 2 (one per container)
	if strings.Count(patch.Patch, "REDIS_HOST") != 2 {
		t.Errorf("expected REDIS_HOST to appear 2 times, got %d", strings.Count(patch.Patch, "REDIS_HOST"))
	}
	if strings.Count(patch.Patch, "OIDC_CLIENT_SECRET") != 2 {
		t.Errorf("expected OIDC_CLIENT_SECRET to appear 2 times, got %d", strings.Count(patch.Patch, "OIDC_CLIENT_SECRET"))
	}
}

func TestGenerateDeploymentEnvPatchesMultiContainer(t *testing.T) {
	envPatches := []EnvPatch{
		{Name: "DATABASE_HOST", Value: "postgres-preview-7-rw.preview-pr-7.svc.cluster.local"},
	}

	patch := generateDeploymentEnvPatches("myapp", []string{"myapp", "myapp-worker"}, envPatches)
	if patch == nil {
		t.Fatal("expected non-nil patch")
	}

	if !strings.Contains(patch.Patch, "name: myapp\n") {
		t.Error("patch missing myapp container")
	}
	if !strings.Contains(patch.Patch, "name: myapp-worker") {
		t.Error("patch missing myapp-worker container")
	}
	if strings.Count(patch.Patch, "DATABASE_HOST") != 2 {
		t.Errorf("expected DATABASE_HOST to appear 2 times, got %d", strings.Count(patch.Patch, "DATABASE_HOST"))
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

func TestBuildAllPatchesHelmWithEnvMapping(t *testing.T) {
	config := &v1.PreviewConfig{
		Spec: v1.PreviewConfigSpec{
			DeploymentType: v1.DeploymentTypeHelm,
			Redis:          &v1.RedisConfig{Enabled: true},
			HelmValues: &v1.HelmValuesMapping{
				DatabaseHost:     "externalDatabase.host",
				DatabaseName:     "externalDatabase.database",
				DatabasePassword: "externalDatabase.password",
				AppURL:           "nextcloud.host",
			},
			EnvMapping: &v1.EnvMapping{
				ContainerNames:   []string{"nextcloud", "nextcloud-cron"},
				RedisHost:        "REDIS_HOST",
				OIDCClientSecret: "OIDC_CLIENT_SECRET",
			},
		},
	}

	patches := buildAllPatches("nextcloud", "8", "preview-pr-8", "daddyshome.fr", config, "/apps/user_oidc/code")

	// Should have strip patches + valuesFrom replace + helm value patch + postRenderer patch
	foundValuesFromReplace := false
	foundHelmValues := false
	foundPostRenderer := false
	for _, p := range patches {
		if p.Target != nil && p.Target.Kind == "HelmRelease" {
			if strings.Contains(p.Patch, "postRenderers") {
				foundPostRenderer = true
				if !strings.Contains(p.Patch, "REDIS_HOST") {
					t.Error("postRenderer missing REDIS_HOST")
				}
				if !strings.Contains(p.Patch, "OIDC_CLIENT_SECRET") {
					t.Error("postRenderer missing OIDC_CLIENT_SECRET")
				}
				if !strings.Contains(p.Patch, "preview-oidc-client-secret-8") {
					t.Error("postRenderer missing OIDC secret ref")
				}
				// Both containers should be patched
				if !strings.Contains(p.Patch, "name: nextcloud-cron") {
					t.Error("postRenderer missing nextcloud-cron container")
				}
				if strings.Count(p.Patch, "REDIS_HOST") != 2 {
					t.Errorf("expected REDIS_HOST 2 times (one per container), got %d", strings.Count(p.Patch, "REDIS_HOST"))
				}
			} else if strings.Contains(p.Patch, "valuesFrom") {
				foundValuesFromReplace = true
				// Should reference CNPG superuser secret for DB password
				if !strings.Contains(p.Patch, "postgres-preview-8-superuser") {
					t.Error("valuesFrom missing CNPG superuser secret reference")
				}
				if !strings.Contains(p.Patch, "targetPath: externalDatabase.password") {
					t.Error("valuesFrom missing password targetPath")
				}
			} else {
				foundHelmValues = true
				if !strings.Contains(p.Patch, "externalDatabase") {
					t.Error("helm values missing externalDatabase")
				}
			}
		}
	}
	if !foundValuesFromReplace {
		t.Error("missing valuesFrom replace patch")
	}
	if !foundHelmValues {
		t.Error("missing HelmRelease value patch")
	}
	if !foundPostRenderer {
		t.Error("missing postRenderer patch")
	}
}

func TestBuildAllPatchesHelmNoDbPassword(t *testing.T) {
	// When no DB password is configured, valuesFrom should be empty
	config := &v1.PreviewConfig{
		Spec: v1.PreviewConfigSpec{
			DeploymentType: v1.DeploymentTypeHelm,
			HelmValues: &v1.HelmValuesMapping{
				AppURL: "app.url",
			},
		},
	}

	patches := buildAllPatches("myapp", "1", "preview-pr-1", "daddyshome.fr", config, "/oauth/callback")

	foundEmptyValuesFrom := false
	for _, p := range patches {
		if p.Target != nil && p.Target.Kind == "HelmRelease" && strings.Contains(p.Patch, "valuesFrom: []") {
			foundEmptyValuesFrom = true
		}
	}
	if !foundEmptyValuesFrom {
		t.Error("expected empty valuesFrom when no DB password configured")
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

func TestIndentYAML(t *testing.T) {
	input := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: test"
	result := indentYAML(input, 4)
	expected := "    apiVersion: v1\n    kind: Pod\n    metadata:\n      name: test"
	if result != expected {
		t.Errorf("indentYAML mismatch:\ngot:  %q\nwant: %q", result, expected)
	}
}
