package controller

import (
	"context"
	"fmt"
	"strings"

	kustomize "github.com/fluxcd/pkg/apis/kustomize"
	corev1 "k8s.io/api/core/v1"
	sigsyaml "sigs.k8s.io/yaml"

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
	// Prod's static PersistentVolumes are cluster-scoped; a prune of the preview must never delete them.
	{Group: "", Kind: "PersistentVolume"},
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
			// valueFrom: null clears any existing valueFrom (e.g. a secretKeyRef
			// to a stripped ExternalSecret) so a plain override wins cleanly.
			envLines = append(envLines, fmt.Sprintf(
				"                          - name: %s\n                            value: %q\n                            valueFrom: null",
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

// DBCredentials holds the database username and password resolved from the
// CNPG superuser secret. Passed to buildAllPatches so Helm value patches
// can embed the real credentials via strategic merge.
type DBCredentials struct {
	Username string
	Password string
}

// generateValuesFromStripPatch creates a strategic merge patch that empties valuesFrom
// on a HelmRelease. In preview, ExternalSecrets are stripped, so secrets referenced by
// valuesFrom won't exist. DB credentials are injected directly via Helm value patches.
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
				"            - name: %s\n              value: %q\n              valueFrom: null", ep.Name, ep.Value))
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

// generateContainerStripPatch deletes named containers / initContainers from the
// previewed Deployment via strategic-merge `$patch: delete`. Used to remove a
// backup sidecar (e.g. litestream) so a preview can't replicate cloned data to a
// production bucket.
func generateContainerStripPatch(appName string, removeContainers, removeInitContainers []string) *kustomize.Patch {
	if len(removeContainers) == 0 && len(removeInitContainers) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: %s\nspec:\n  template:\n    spec:\n", appName)
	if len(removeContainers) > 0 {
		b.WriteString("      containers:\n")
		for _, c := range removeContainers {
			fmt.Fprintf(&b, "        - name: %s\n          $patch: delete\n", c)
		}
	}
	if len(removeInitContainers) > 0 {
		b.WriteString("      initContainers:\n")
		for _, c := range removeInitContainers {
			fmt.Fprintf(&b, "        - name: %s\n          $patch: delete\n", c)
		}
	}
	return &kustomize.Patch{
		Target: &kustomize.Selector{Kind: "Deployment", Name: appName},
		Patch:  b.String(),
	}
}

// generateNFSConfigVolumePatch replaces the app's config PVC volume with an emptyDir and,
// when nfs.Copy is set, adds the config-seed initContainer that fills it from the NFS export.
func generateNFSConfigVolumePatch(appName, prNumber, namespace, previewDomain string, nfs *nfsConfigVolume) (*kustomize.Patch, error) {
	if nfs == nil {
		return nil, nil
	}
	volumes := []map[string]interface{}{{
		"name":                  nfs.VolumeName,
		"persistentVolumeClaim": nil,
		"emptyDir":              map[string]interface{}{"sizeLimit": nfs.SizeLimit.String()},
	}}
	podSpec := map[string]interface{}{"volumes": volumes}

	if nfs.Copy {
		volumes = append(volumes, map[string]interface{}{
			"name": nfsSeedSourceVolume,
			"nfs":  map[string]interface{}{"server": nfs.Server, "path": nfs.Path, "readOnly": true},
		})
		podSpec["volumes"] = volumes
		podSpec["initContainers"] = []map[string]interface{}{nfsSeedInitContainer(appName, prNumber, namespace, previewDomain, nfs)}
	}

	doc := map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]interface{}{"name": appName},
		"spec":       map[string]interface{}{"template": map[string]interface{}{"spec": podSpec}},
	}
	out, err := sigsyaml.Marshal(doc)
	if err != nil {
		// A dropped patch would leave the production NFS claim mounted read-write.
		return nil, fmt.Errorf("render the mode: nfs patch for Deployment %q: %w", appName, err)
	}
	return &kustomize.Patch{
		Target: &kustomize.Selector{Kind: "Deployment", Name: appName},
		Patch:  string(out),
	}, nil
}

func nfsSeedInitContainer(appName, prNumber, namespace, previewDomain string, nfs *nfsConfigVolume) map[string]interface{} {
	limitKiB := nfs.SizeLimit.Value() / 1024
	env := []map[string]interface{}{
		{"name": "PROD_NS", "value": appName},
		{"name": "PREVIEW_NS", "value": namespace},
		{"name": "DOMAIN", "value": previewDomain},
		{"name": "PR", "value": prNumber},
		{"name": "APP", "value": appName},
		{"name": "SOURCE", "value": nfs.Server + ":" + nfs.Path},
		{"name": "LIMIT", "value": nfs.SizeLimit.String()},
		{"name": "LIMIT_KIB", "value": fmt.Sprintf("%d", limitKiB)},
	}
	if nfs.RunAsUser != nil {
		gid := *nfs.RunAsUser
		if nfs.RunAsGroup != nil {
			gid = *nfs.RunAsGroup
		}
		env = append(env,
			map[string]interface{}{"name": "SEED_UID", "value": fmt.Sprintf("%d", *nfs.RunAsUser)},
			map[string]interface{}{"name": "SEED_GID", "value": fmt.Sprintf("%d", gid)},
		)
	}
	return map[string]interface{}{
		"name":                     nfsSeedContainer,
		"image":                    "alpine:3.21",
		"command":                  []string{"/bin/sh", "-c", nfsSeedScript},
		"env":                      env,
		"terminationMessagePolicy": string(corev1.TerminationMessageFallbackToLogsOnError),
		// runAsNonRoot false: the app pod may set it pod-wide, and the seed must run as root to read the export.
		"securityContext": map[string]interface{}{"runAsUser": 0, "runAsNonRoot": false},
		"volumeMounts": []map[string]interface{}{
			{"name": nfs.VolumeName, "mountPath": "/config"},
			{"name": nfsSeedSourceVolume, "mountPath": "/src", "readOnly": true},
		},
		"resources": map[string]interface{}{
			// The copy lands in the emptyDir (bounded by its sizeLimit); this only covers the container layer and logs.
			"requests": map[string]interface{}{"cpu": "50m", "memory": "64Mi", "ephemeral-storage": "16Mi"},
			"limits":   map[string]interface{}{"cpu": "200m", "memory": "128Mi", "ephemeral-storage": "64Mi"},
		},
	}
}

// buildVolumePatches resolves the cluster state the NFS options need and renders their
// Deployment patches: the configVolume mode: nfs swap and the nfsVolumes mounts.
func (h *PreviewHandler) buildVolumePatches(ctx context.Context, appName, prNumber, namespace string, config *v1.PreviewConfig) ([]kustomize.Patch, error) {
	var patches []kustomize.Patch
	nfs, err := h.resolveNFSConfigVolume(ctx, appName, config)
	if err != nil {
		return nil, err
	}
	if nfs != nil {
		patch, err := generateNFSConfigVolumePatch(appName, prNumber, namespace, h.previewDomain, nfs)
		if err != nil {
			return nil, err
		}
		patches = append(patches, *patch)
	}
	mounts, err := h.resolveNFSVolumes(ctx, appName, config)
	if err != nil {
		return nil, err
	}
	if patch, err := generateNFSVolumesPatch(appName, mounts); err != nil {
		return nil, err
	} else if patch != nil {
		patches = append(patches, *patch)
	}
	return patches, nil
}

// buildAllPatches combines all patch types for a preview Kustomization
func buildAllPatches(appName, prNumber, namespace, previewDomain string, config *v1.PreviewConfig, callbackPath string, dbCreds *DBCredentials, volumePatches []kustomize.Patch) []kustomize.Patch {
	var allPatches []kustomize.Patch

	// 1. Infrastructure strip patches (remove resources not needed in preview)
	allPatches = append(allPatches, generateStripPatches()...)
	allPatches = append(allPatches, generateNFSClaimStripPatches(config.Spec.NFSVolumes)...)

	// 2. App-specific patches based on deployment type
	switch config.Spec.DeploymentType {
	case v1.DeploymentTypeHelm:
		h := &PreviewHandler{previewDomain: previewDomain}

		// Strip valuesFrom — ExternalSecrets are removed in preview, so
		// secrets referenced by valuesFrom won't exist. DB credentials are
		// injected directly via Helm value patches instead.
		allPatches = append(allPatches, *generateValuesFromStripPatch(appName))

		// Helm value patches (simple dot-paths for HelmRelease spec.values)
		if config.Spec.HelmValues != nil {
			valuePatches := h.buildHelmValuePatches(namespace, appName, prNumber, config, dbCreds)
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
		// Strip backup/sidecar containers (e.g. litestream) so the preview can't
		// write the cloned data back to a production bucket.
		if config.Spec.DeploymentPatch != nil {
			if p := generateContainerStripPatch(appName,
				config.Spec.DeploymentPatch.RemoveContainers,
				config.Spec.DeploymentPatch.RemoveInitContainers); p != nil {
				allPatches = append(allPatches, *p)
			}
		}

		allPatches = append(allPatches, volumePatches...)

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
