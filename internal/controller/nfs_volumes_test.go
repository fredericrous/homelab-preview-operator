package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	sigsyaml "sigs.k8s.io/yaml"

	v1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

const mediaManifestYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: radarr
spec:
  template:
    spec:
      containers:
      - name: radarr
        image: radarr:1
        volumeMounts:
        - name: media
          mountPath: /media
        - name: media
          mountPath: /downloads
          subPath: Downloads
        - name: media2
          mountPath: /media2
      volumes:
      - name: media
        persistentVolumeClaim:
          claimName: media
      - name: media2
        persistentVolumeClaim:
          claimName: media2
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: media
spec:
  volumeName: radarr-media-pv
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: media2
spec:
  volumeName: radarr-media2-pv
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: radarr-media-pv
spec:
  nfs:
    server: nas.local
    path: /export/media
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: radarr-media2-pv
spec:
  nfs:
    server: nas.local
    path: /export/media2
`

func radarrObjects() []client.Object {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "radarr", Namespace: "radarr"}}
	deployment.Spec.Template.Spec.Volumes = []corev1.Volume{
		{Name: "media", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "media"}}},
		{Name: "media2", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "media2"}}},
	}
	claim := func(name, volume string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "radarr"},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: volume},
		}
	}
	volume := func(name, path string) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: "nas.local", Path: path},
			}},
		}
	}
	return []client.Object{
		deployment,
		claim("media", "radarr-media-pv"), claim("media2", "radarr-media2-pv"),
		volume("radarr-media-pv", "/export/media"), volume("radarr-media2-pv", "/export/media2"),
	}
}

func mediaConfig() *v1.PreviewConfig {
	return &v1.PreviewConfig{Spec: v1.PreviewConfigSpec{
		NFSVolumes: []v1.NFSVolume{{ClaimName: "media"}, {ClaimName: "media2"}},
	}}
}

// renderMedia builds the patches for config the way PrepareKustomization does and applies them
// to mediaManifestYAML with krusty.
func renderMedia(t *testing.T, config *v1.PreviewConfig, manifest string, objects []client.Object) []map[string]interface{} {
	t.Helper()
	h := nfsTestHandler(t, objects...)
	volumePatches, err := h.buildVolumePatches(context.Background(), "radarr", "7", "preview-pr-7", config)
	if err != nil {
		t.Fatal(err)
	}
	patches := buildAllPatches("radarr", "7", "preview-pr-7", "example.test", config, "", nil, volumePatches)
	var kustomization struct {
		Resources []string                 `json:"resources"`
		Patches   []map[string]interface{} `json:"patches"`
	}
	kustomization.Resources = []string{"manifest.yaml"}
	for _, patch := range patches {
		entry := map[string]interface{}{"patch": patch.Patch}
		if patch.Target != nil {
			entry["target"] = map[string]interface{}{"kind": patch.Target.Kind, "name": patch.Target.Name, "group": patch.Target.Group}
		}
		kustomization.Patches = append(kustomization.Patches, entry)
	}
	kustomizationYAML, err := sigsyaml.Marshal(kustomization)
	if err != nil {
		t.Fatal(err)
	}
	fs := filesys.MakeFsInMemory()
	if err := fs.WriteFile("/kustomization.yaml", kustomizationYAML); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/manifest.yaml", []byte(manifest)); err != nil {
		t.Fatal(err)
	}
	resMap, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(fs, "/")
	if err != nil {
		t.Fatalf("kustomize build: %v", err)
	}
	var rendered []map[string]interface{}
	for _, resource := range resMap.Resources() {
		out, err := resource.AsYAML()
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]interface{}
		if err := sigsyaml.Unmarshal(out, &object); err != nil {
			t.Fatal(err)
		}
		rendered = append(rendered, object)
	}
	return rendered
}

func TestNFSVolumesRenderReadOnlyInlineVolumesAndStripClaims(t *testing.T) {
	rendered := renderMedia(t, mediaConfig(), mediaManifestYAML, radarrObjects())
	if len(rendered) != 1 || rendered[0]["kind"] != "Deployment" {
		t.Fatalf("PVCs and PVs must be deleted, left: %v", rendered)
	}
	podSpec := rendered[0]["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	volumes := volumesByName(podSpec)
	for name, path := range map[string]string{"media": "/export/media", "media2": "/export/media2"} {
		if _, has := volumes[name]["persistentVolumeClaim"]; has {
			t.Fatalf("%s keeps its persistentVolumeClaim: %v", name, volumes[name])
		}
		source, _ := volumes[name]["nfs"].(map[string]interface{})
		if source["server"] != "nas.local" || source["path"] != path || source["readOnly"] != true {
			t.Fatalf("%s nfs = %v", name, source)
		}
	}
	mounts := podSpec["containers"].([]interface{})[0].(map[string]interface{})["volumeMounts"].([]interface{})
	if len(mounts) != 3 {
		t.Fatalf("mounts = %v", mounts)
	}
	subPath := mounts[1].(map[string]interface{})
	if subPath["name"] != "media" || subPath["mountPath"] != "/downloads" || subPath["subPath"] != "Downloads" {
		t.Fatalf("subPath mount changed: %v", subPath)
	}
	if mounts[0].(map[string]interface{})["readOnly"] != nil {
		t.Fatalf("container mounts must be left as rendered: %v", mounts[0])
	}
}

func TestNFSVolumesCombineWithConfigVolumeNFSMode(t *testing.T) {
	objects := radarrObjects()
	deployment := objects[0].(*appsv1.Deployment)
	deployment.Spec.Template.Spec.Volumes = append(deployment.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "config", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "config"}},
	})
	manifest := strings.Replace(mediaManifestYAML, "      volumes:\n", "      volumes:\n      - name: config\n        persistentVolumeClaim:\n          claimName: config\n", 1)
	config := mediaConfig()
	config.Spec.ConfigVolume = &v1.ConfigVolumeConfig{ClaimName: "config", Mode: v1.ConfigVolumeModeNFS, NFS: &v1.NFSConfigVolume{Copy: boolPtr(false)}}

	rendered := renderMedia(t, config, manifest, objects)
	podSpec := rendered[0]["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	volumes := volumesByName(podSpec)
	if _, has := volumes["config"]["emptyDir"]; !has {
		t.Fatalf("config must be a temp dir: %v", volumes["config"])
	}
	for _, name := range []string{"media", "media2"} {
		source, _ := volumes[name]["nfs"].(map[string]interface{})
		if source["readOnly"] != true || volumes[name]["persistentVolumeClaim"] != nil {
			t.Fatalf("%s = %v", name, volumes[name])
		}
	}
}

func TestPersistentVolumeIsAStripTarget(t *testing.T) {
	for _, patch := range generateStripPatches() {
		if patch.Target.Kind == "PersistentVolume" && patch.Target.Group == "" {
			return
		}
	}
	t.Fatal("PersistentVolume must be stripped from every preview")
}

func TestResolveNFSVolumesErrors(t *testing.T) {
	helm := mediaConfig()
	helm.Spec.DeploymentType = v1.DeploymentTypeHelm
	both := mediaConfig()
	both.Spec.ConfigVolume = &v1.ConfigVolumeConfig{ClaimName: "media"}
	unknown := &v1.PreviewConfig{Spec: v1.PreviewConfigSpec{NFSVolumes: []v1.NFSVolume{{ClaimName: "other"}}}}

	unbound := radarrObjects()
	unbound[1] = &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "media", Namespace: "radarr"}}
	notNFS := radarrObjects()
	notNFS[3] = &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "radarr-media-pv"}}

	cases := []struct {
		name    string
		objects []client.Object
		config  *v1.PreviewConfig
		want    string
	}{
		{"claim not used", radarrObjects(), unknown, `nfsVolumes claim "other" is not used as a persistentVolumeClaim volume by Deployment "radarr"`},
		{"PVC not bound", unbound, mediaConfig(), `nfsVolumes claim "media" in namespace "radarr" is not bound to a PV`},
		{"PV without nfs", notNFS, mediaConfig(), `nfsVolumes claim "media" is bound to PV "radarr-media-pv", which has no spec.nfs`},
		{"claim in both", radarrObjects(), both, `claim "media" is in both configVolume.claimName and nfsVolumes`},
		{"helm", radarrObjects(), helm, "nfsVolumes supports raw-manifest Deployments only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := nfsTestHandler(t, tc.objects...)
			_, err := h.resolveNFSVolumes(context.Background(), "radarr", tc.config)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if strings.HasSuffix(err.Error(), ".") {
				t.Fatalf("error %q ends with punctuation", err)
			}
		})
	}
}

func TestProcessRejectsNFSVolumes(t *testing.T) {
	config := mediaConfig()
	config.Name, config.Namespace = "radarr", "radarr"
	h := nfsTestHandler(t, config)
	ks := newSuspendedKustomization()
	err := h.Process(context.Background(), ks, "radarr")
	if err == nil || !strings.Contains(err.Error(), "nfsVolumes is supported by the Kustomization flow only") {
		t.Fatalf("error = %v", err)
	}
}

func hashOf(spec string) string {
	sum := sha256.Sum256([]byte(spec))
	return hex.EncodeToString(sum[:])
}

func TestSpecWithoutNFSOptionsHashesAsBefore(t *testing.T) {
	spec := v1.PreviewConfigSpec{DeploymentType: v1.DeploymentTypeDeployment, SharedServices: []string{"litellm"}}
	// sha256 of the JSON the spec marshalled to before nfsVolumes and isolateEgress existed.
	const before = `{"deploymentType":"deployment","sharedServices":["litellm"]}`
	if got, want := hashPreviewConfigSpec(&v1.PreviewConfig{Spec: spec}), hashOf(before); got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
}
