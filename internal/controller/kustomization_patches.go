package controller

import (
	"fmt"
	"strings"

	kustomize "github.com/fluxcd/pkg/apis/kustomize"
	corev1 "k8s.io/api/core/v1"

	v1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

// infrastructureStripTarget defines a GVK to remove from preview namespaces
type infrastructureStripTarget struct {
	Group string
	Kind  string
}

// stripTargets is the hardcoded list of resource kinds to remove from preview
var stripTargets = []infrastructureStripTarget{
	{Group: "", Kind: "Namespace"},
	{Group: "external-secrets.io", Kind: "SecretStore"},
	{Group: "external-secrets.io", Kind: "ExternalSecret"},
	{Group: "external-secrets.io", Kind: "PushSecret"},
	{Group: "cert-manager.io", Kind: "Certificate"},
	{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute"},
	{Group: "connectivity.homelab.io", Kind: "DDNSRecord"},
	{Group: "security.istio.io", Kind: "PeerAuthentication"},
	{Group: "redhatcop.redhat.io", Kind: "Policy"},
	{Group: "redhatcop.redhat.io", Kind: "KubernetesAuthEngineRole"},
	{Group: "gateway.envoyproxy.io", Kind: "SecurityPolicy"},
	{Group: "gateway.envoyproxy.io", Kind: "ClientTrafficPolicy"},
	{Group: "security.homelab.io", Kind: "OIDCClient"},
	{Group: "batch", Kind: "Job"},
	{Group: "generators.external-secrets.io", Kind: "Password"},
	{Group: "postgresql.cnpg.io", Kind: "Database"},
}

// generateStripPatches produces $patch: delete patches for all infrastructure resources
// that should be excluded from preview namespaces.
func generateStripPatches() []kustomize.Patch {
	patches := make([]kustomize.Patch, 0, len(stripTargets))
	for _, target := range stripTargets {
		var apiVersion string
		if target.Group != "" {
			apiVersion = target.Group + "/v1"
		} else {
			apiVersion = "v1"
		}

		patch := fmt.Sprintf(
			"apiVersion: %s\nkind: %s\nmetadata:\n  name: placeholder\n$patch: delete",
			apiVersion, target.Kind,
		)

		patches = append(patches, kustomize.Patch{
			Target: &kustomize.Selector{
				Group: target.Group,
				Kind:  target.Kind,
			},
			Patch: patch,
		})
	}
	return patches
}

// generateDeploymentEnvPatches creates a strategic merge patch for a Deployment
// that overrides environment variables for the preview. Multiple container names
// are supported — each container gets the same env overrides.
func generateDeploymentEnvPatches(appName string, containerNames []string, envPatches []EnvPatch) *kustomize.Patch {
	if len(envPatches) == 0 {
		return nil
	}

	if len(containerNames) == 0 {
		containerNames = []string{appName}
	}

	var envLines []string
	for _, ep := range envPatches {
		if ep.ValueFrom != nil {
			envLines = append(envLines, buildEnvVarFromYAML(ep))
		} else {
			envLines = append(envLines, fmt.Sprintf(
				"                          - name: %s\n                            value: %q",
				ep.Name, ep.Value,
			))
		}
	}
	envBlock := strings.Join(envLines, "\n")

	var containerEntries []string
	for _, cn := range containerNames {
		containerEntries = append(containerEntries, fmt.Sprintf(
			"        - name: %s\n          env:\n%s", cn, envBlock))
	}

	patch := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
spec:
  template:
    spec:
      containers:
%s`, appName, strings.Join(containerEntries, "\n"))

	return &kustomize.Patch{
		Target: &kustomize.Selector{
			Kind: "Deployment",
			Name: appName,
		},
		Patch: patch,
	}
}

// buildEnvVarFromYAML renders a single env var with valueFrom as YAML
func buildEnvVarFromYAML(ep EnvPatch) string {
	if ep.ValueFrom != nil && ep.ValueFrom.SecretKeyRef != nil {
		return fmt.Sprintf(
			"                          - name: %s\n                            valueFrom:\n                              secretKeyRef:\n                                name: %s\n                                key: %s",
			ep.Name, ep.ValueFrom.SecretKeyRef.Name, ep.ValueFrom.SecretKeyRef.Key,
		)
	}
	// Fallback: plain value
	return fmt.Sprintf(
		"                          - name: %s\n                            value: %q",
		ep.Name, ep.Value,
	)
}

// generateValuesFromStripPatch creates a strategic merge patch that empties valuesFrom
// on a HelmRelease. In preview, ExternalSecrets are stripped, so secrets referenced by
// valuesFrom won't exist. Env var overrides (via postRenderers) handle the values instead.
func generateValuesFromStripPatch(appName string) *kustomize.Patch {
	patch := fmt.Sprintf(`apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: %s
spec:
  valuesFrom: []`, appName)

	return &kustomize.Patch{
		Target: &kustomize.Selector{
			Group: "helm.toolkit.fluxcd.io",
			Kind:  "HelmRelease",
			Name:  appName,
		},
		Patch: patch,
	}
}

// generateHelmValuePatches creates a strategic merge patch for a HelmRelease.
// Only supports simple dot-notation paths. For env var overrides, use
// generatePostRendererPatch instead.
func generateHelmValuePatches(appName string, valuePatches map[string]string) *kustomize.Patch {
	if len(valuePatches) == 0 {
		return nil
	}

	root := make(map[string]interface{})
	for path, value := range valuePatches {
		// Skip array-indexed paths — these should use envMapping with postRenderers
		if strings.Contains(path, "[") {
			continue
		}
		setNestedMapValue(root, path, value)
	}

	if len(root) == 0 {
		return nil
	}

	valuesYAML := renderYAMLMap(root, 4)
	patch := fmt.Sprintf(`apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: %s
spec:
  values:
%s`, appName, valuesYAML)

	return &kustomize.Patch{
		Target: &kustomize.Selector{
			Group: "helm.toolkit.fluxcd.io",
			Kind:  "HelmRelease",
			Name:  appName,
		},
		Patch: patch,
	}
}

// generatePostRendererPatch creates a HelmRelease patch that adds a postRenderer
// to override container env vars by name. This is used instead of patching Helm
// values array indices (e.g., extraEnv[0].value) which are fragile.
// The postRenderer applies a strategic merge on the rendered Deployment, using
// the env var name as the merge key. Multiple container names are supported —
// each container gets the same env overrides.
func generatePostRendererPatch(appName string, containerNames []string, envPatches []EnvPatch) *kustomize.Patch {
	if len(envPatches) == 0 {
		return nil
	}

	if len(containerNames) == 0 {
		containerNames = []string{appName}
	}

	// Build env YAML lines (shared across all containers)
	var envLines []string
	for _, ep := range envPatches {
		if ep.ValueFrom != nil && ep.ValueFrom.SecretKeyRef != nil {
			envLines = append(envLines, fmt.Sprintf(
				"            - name: %s\n              valueFrom:\n                secretKeyRef:\n                  name: %s\n                  key: %s",
				ep.Name, ep.ValueFrom.SecretKeyRef.Name, ep.ValueFrom.SecretKeyRef.Key))
		} else {
			envLines = append(envLines, fmt.Sprintf(
				"            - name: %s\n              value: %q", ep.Name, ep.Value))
		}
	}
	envBlock := strings.Join(envLines, "\n")

	// Build container entries for the inner Deployment strategic merge patch
	var containerEntries []string
	for _, cn := range containerNames {
		containerEntries = append(containerEntries, fmt.Sprintf(
			"        - name: %s\n          env:\n%s", cn, envBlock))
	}

	// Build the inner Deployment strategic merge patch
	innerPatch := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
spec:
  template:
    spec:
      containers:
%s`, appName, strings.Join(containerEntries, "\n"))

	// Indent inner patch for embedding as a YAML block scalar (14 spaces)
	indentedPatch := indentYAML(innerPatch, 14)

	patch := fmt.Sprintf(`apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: %s
spec:
  postRenderers:
    - kustomize:
        patches:
          - patch: |
%s`, appName, indentedPatch)

	return &kustomize.Patch{
		Target: &kustomize.Selector{
			Group: "helm.toolkit.fluxcd.io",
			Kind:  "HelmRelease",
			Name:  appName,
		},
		Patch: patch,
	}
}

// indentYAML adds the given number of spaces to the beginning of each non-empty line
func indentYAML(s string, spaces int) string {
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(s, "\n")
	for i := range lines {
		if lines[i] != "" {
			lines[i] = prefix + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// buildAllPatches combines all patch types for a preview Kustomization
func buildAllPatches(appName, prNumber, namespace, previewDomain string, config *v1.PreviewConfig, callbackPath string) []kustomize.Patch {
	var allPatches []kustomize.Patch

	// 1. Infrastructure strip patches (remove resources not needed in preview)
	allPatches = append(allPatches, generateStripPatches()...)

	// 2. App-specific patches based on deployment type
	switch config.Spec.DeploymentType {
	case v1.DeploymentTypeHelm:
		h := &PreviewHandler{previewDomain: previewDomain}

		// Strip valuesFrom — ExternalSecrets are removed in preview, so
		// secrets referenced by valuesFrom won't exist. Values are handled
		// via Helm value patches and env var overrides instead.
		allPatches = append(allPatches, *generateValuesFromStripPatch(appName))

		// Helm value patches (simple dot-paths for HelmRelease spec.values)
		if config.Spec.HelmValues != nil {
			valuePatches := h.buildHelmValuePatches(namespace, appName, prNumber, config)
			if p := generateHelmValuePatches(appName, valuePatches); p != nil {
				allPatches = append(allPatches, *p)
			}
		}

		// Env var overrides via postRenderers (for values injected as container env vars)
		if config.Spec.EnvMapping != nil {
			envPatches := h.buildEnvPatches(namespace, appName, prNumber, config)

			// Override OIDC secret name to include PR number
			for i := range envPatches {
				if envPatches[i].Name == config.Spec.EnvMapping.OIDCClientSecret {
					envPatches[i].ValueFrom = &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: fmt.Sprintf("preview-oidc-client-secret-%s", prNumber),
							},
							Key: "client_secret",
						},
					}
				}
			}

			containerNames := config.Spec.EnvMapping.ContainerNames
			if p := generatePostRendererPatch(appName, containerNames, envPatches); p != nil {
				allPatches = append(allPatches, *p)
			}
		}

	default:
		if config.Spec.EnvMapping != nil {
			h := &PreviewHandler{previewDomain: previewDomain}
			envPatches := h.buildEnvPatches(namespace, appName, prNumber, config)

			// Update OIDC secret references to use operator-created secret
			for i := range envPatches {
				if envPatches[i].Name == config.Spec.EnvMapping.OIDCClientSecret {
					envPatches[i].ValueFrom = &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: fmt.Sprintf("preview-oidc-client-secret-%s", prNumber),
							},
							Key: "client_secret",
						},
					}
				}
			}

			containerNames := config.Spec.EnvMapping.ContainerNames
			if p := generateDeploymentEnvPatches(appName, containerNames, envPatches); p != nil {
				allPatches = append(allPatches, *p)
			}
		}
	}

	return allPatches
}

// setNestedMapValue sets a value in a nested map using dot-notation path
func setNestedMapValue(m map[string]interface{}, path string, value string) {
	parts := strings.Split(path, ".")
	current := m
	for _, part := range parts[:len(parts)-1] {
		if next, ok := current[part].(map[string]interface{}); ok {
			current = next
		} else {
			newMap := make(map[string]interface{})
			current[part] = newMap
			current = newMap
		}
	}
	current[parts[len(parts)-1]] = value
}

// renderYAMLMap renders a map as indented YAML lines
func renderYAMLMap(m map[string]interface{}, indent int) string {
	var lines []string
	prefix := strings.Repeat(" ", indent)
	for k, v := range m {
		switch val := v.(type) {
		case map[string]interface{}:
			lines = append(lines, fmt.Sprintf("%s%s:", prefix, k))
			lines = append(lines, renderYAMLMap(val, indent+2))
		case string:
			lines = append(lines, fmt.Sprintf("%s%s: %q", prefix, k, val))
		default:
			lines = append(lines, fmt.Sprintf("%s%s: %v", prefix, k, val))
		}
	}
	return strings.Join(lines, "\n")
}
