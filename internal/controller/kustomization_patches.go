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
}

// generateStripPatches produces $patch: delete patches for all infrastructure resources
// that should be excluded from preview namespaces.
func generateStripPatches() []kustomize.Patch {
	patches := make([]kustomize.Patch, 0, len(stripTargets))
	for _, target := range stripTargets {
		apiVersion := target.Kind
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
// that overrides environment variables for the preview.
func generateDeploymentEnvPatches(appName, containerName string, envPatches []EnvPatch) *kustomize.Patch {
	if len(envPatches) == 0 {
		return nil
	}

	if containerName == "" {
		containerName = appName
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

	patch := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
spec:
  template:
    spec:
      containers:
        - name: %s
          env:
%s`, appName, containerName, strings.Join(envLines, "\n"))

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

// generateHelmValuePatches creates a strategic merge patch for a HelmRelease
// that overrides spec.values entries.
func generateHelmValuePatches(appName string, valuePatches map[string]string) *kustomize.Patch {
	if len(valuePatches) == 0 {
		return nil
	}

	// Build a nested YAML structure from dot-notation paths
	root := make(map[string]interface{})
	for path, value := range valuePatches {
		setNestedMapValue(root, path, value)
	}

	// Serialize the values map to YAML
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

// generateOIDCPatches creates patches for OIDC configuration in the app.
// For deployments, this is handled via env patches. For Helm, via value patches.
// This generates the OIDCClient secretRef patch for pointing to the operator-created secret.
func generateOIDCSecretRefPatch(appName, prNumber string) *kustomize.Patch {
	secretName := fmt.Sprintf("preview-oidc-client-secret-%s", prNumber)

	patch := fmt.Sprintf(`apiVersion: security.homelab.io/v1alpha1
kind: OIDCClient
metadata:
  name: %s
spec:
  secretRef:
    name: %s
    key: client_secret`, appName, secretName)

	return &kustomize.Patch{
		Target: &kustomize.Selector{
			Group: "security.homelab.io",
			Kind:  "OIDCClient",
			Name:  appName,
		},
		Patch: patch,
	}
}

// buildAllPatches combines all patch types for a preview Kustomization
func buildAllPatches(appName, prNumber, namespace, previewDomain string, config *v1.PreviewConfig, callbackPath string) []kustomize.Patch {
	var allPatches []kustomize.Patch

	// 1. Infrastructure strip patches (remove resources not needed in preview)
	allPatches = append(allPatches, generateStripPatches()...)

	// 2. App-specific patches based on deployment type
	switch config.Spec.DeploymentType {
	case v1.DeploymentTypeHelm:
		if config.Spec.HelmValues != nil {
			h := &PreviewHandler{previewDomain: previewDomain}
			valuePatches := h.buildHelmValuePatches(namespace, appName, prNumber, config)
			if p := generateHelmValuePatches(appName, valuePatches); p != nil {
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

			containerName := appName
			if p := generateDeploymentEnvPatches(appName, containerName, envPatches); p != nil {
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
			// Quote values that might be interpreted as non-strings
			lines = append(lines, fmt.Sprintf("%s%s: %q", prefix, k, val))
		default:
			lines = append(lines, fmt.Sprintf("%s%s: %v", prefix, k, val))
		}
	}
	return strings.Join(lines, "\n")
}
