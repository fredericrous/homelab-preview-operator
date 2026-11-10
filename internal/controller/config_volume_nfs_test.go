package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
	kustomize "github.com/fluxcd/pkg/apis/kustomize"
	logr "github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	sigsyaml "sigs.k8s.io/yaml"

	v1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

const sampleDeploymentYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  template:
    spec:
      containers:
      - name: myapp
        image: myapp:1
        volumeMounts:
        - name: config
          mountPath: /etc/myapp
      volumes:
      - name: config
        persistentVolumeClaim:
          claimName: myapp-config
`

func boolPtr(b bool) *bool { return &b }

func nfsConfig(copySeed bool) *v1.PreviewConfig {
	return &v1.PreviewConfig{Spec: v1.PreviewConfigSpec{
		ConfigVolume: &v1.ConfigVolumeConfig{
			ClaimName: "myapp-config",
			Mode:      v1.ConfigVolumeModeNFS,
			NFS:       &v1.NFSConfigVolume{Copy: boolPtr(copySeed)},
		},
	}}
}

func nfsTestHandler(t *testing.T, objs ...client.Object) *PreviewHandler {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, v1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &PreviewHandler{client: c, log: logr.Discard(), previewDomain: "example.test"}
}

func prodDeployment(t *testing.T) *appsv1.Deployment {
	t.Helper()
	var deployment appsv1.Deployment
	if err := sigsyaml.Unmarshal([]byte(sampleDeploymentYAML), &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Namespace = "myapp"
	uid := int64(1000)
	deployment.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: &uid}
	return &deployment
}

func prodClaim(volumeName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp-config", Namespace: "myapp"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: volumeName},
	}
}

func nfsPersistentVolume(name string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			NFS: &corev1.NFSVolumeSource{Server: "nas.local", Path: "/export/myapp"},
		}},
	}
}

// renderPodSpec applies the Kustomization patches to the sample Deployment the way Flux does
// and returns the rendered pod spec.
func renderPodSpec(t *testing.T, config *v1.PreviewConfig, nfs *nfsConfigVolume) map[string]interface{} {
	t.Helper()
	nfsPatch, err := generateNFSConfigVolumePatch("myapp", "7", "preview-pr-7", "example.test", nfs)
	if err != nil {
		t.Fatal(err)
	}
	patches := buildAllPatches("myapp", "7", "preview-pr-7", "example.test", config, "", nil, []kustomize.Patch{*nfsPatch})
	var kustomization struct {
		Resources []string                 `json:"resources"`
		Patches   []map[string]interface{} `json:"patches"`
	}
	kustomization.Resources = []string{"deployment.yaml"}
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
	if err := fs.WriteFile("/deployment.yaml", []byte(sampleDeploymentYAML)); err != nil {
		t.Fatal(err)
	}
	resMap, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(fs, "/")
	if err != nil {
		t.Fatalf("kustomize build: %v", err)
	}
	out, err := resMap.AsYaml()
	if err != nil {
		t.Fatal(err)
	}
	var deployment struct {
		Spec struct {
			Template struct {
				Spec map[string]interface{} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := sigsyaml.Unmarshal(out, &deployment); err != nil {
		t.Fatal(err)
	}
	return deployment.Spec.Template.Spec
}

func volumesByName(spec map[string]interface{}) map[string]map[string]interface{} {
	byName := map[string]map[string]interface{}{}
	volumes, _ := spec["volumes"].([]interface{})
	for _, entry := range volumes {
		volume := entry.(map[string]interface{})
		byName[volume["name"].(string)] = volume
	}
	return byName
}

func TestNFSPatchWithoutCopyIsAnEmptyDir(t *testing.T) {
	nfs := &nfsConfigVolume{VolumeName: "config", SizeLimit: resource.MustParse("512Mi")}
	spec := renderPodSpec(t, nfsConfig(false), nfs)

	config := volumesByName(spec)["config"]
	if _, has := config["persistentVolumeClaim"]; has {
		t.Fatalf("persistentVolumeClaim must be gone: %v", config)
	}
	emptyDir, _ := config["emptyDir"].(map[string]interface{})
	if emptyDir["sizeLimit"] != "512Mi" {
		t.Fatalf("emptyDir sizeLimit = %v", emptyDir)
	}
	if _, has := spec["initContainers"]; has {
		t.Fatalf("copy false must add no initContainer: %v", spec["initContainers"])
	}
	if len(volumesByName(spec)) != 1 {
		t.Fatalf("copy false must add no other volume: %v", spec["volumes"])
	}
}

func TestNFSPatchWithCopyAddsReadOnlySourceAndSeed(t *testing.T) {
	uid, gid := int64(1000), int64(2000)
	nfs := &nfsConfigVolume{
		VolumeName: "config", SizeLimit: resource.MustParse("256Mi"), Copy: true,
		Server: "nas.local", Path: "/export/myapp", PVName: "pv-1", RunAsUser: &uid, RunAsGroup: &gid,
	}
	spec := renderPodSpec(t, nfsConfig(true), nfs)

	volumes := volumesByName(spec)
	if _, has := volumes["config"]["persistentVolumeClaim"]; has {
		t.Fatal("persistentVolumeClaim must be gone")
	}
	source, _ := volumes[nfsSeedSourceVolume]["nfs"].(map[string]interface{})
	if source["server"] != "nas.local" || source["path"] != "/export/myapp" || source["readOnly"] != true {
		t.Fatalf("source volume = %v", source)
	}
	inits, _ := spec["initContainers"].([]interface{})
	if len(inits) != 1 {
		t.Fatalf("initContainers = %v", inits)
	}
	seed := inits[0].(map[string]interface{})
	if seed["name"] != nfsSeedContainer || seed["terminationMessagePolicy"] != "FallbackToLogsOnError" {
		t.Fatalf("seed container = %v", seed)
	}
	var sourceMountReadOnly bool
	for _, entry := range seed["volumeMounts"].([]interface{}) {
		mount := entry.(map[string]interface{})
		if mount["name"] == nfsSeedSourceVolume {
			sourceMountReadOnly = mount["readOnly"] == true && mount["mountPath"] == "/src"
		}
	}
	if !sourceMountReadOnly {
		t.Fatalf("source mount must be read-only at /src: %v", seed["volumeMounts"])
	}
	resources := seed["resources"].(map[string]interface{})
	if resources["requests"].(map[string]interface{})["ephemeral-storage"] != "16Mi" ||
		resources["limits"].(map[string]interface{})["ephemeral-storage"] != "64Mi" {
		t.Fatalf("seed container ephemeral-storage = %v", resources)
	}
	security := seed["securityContext"].(map[string]interface{})
	if security["runAsUser"] != float64(0) || security["runAsNonRoot"] != false {
		t.Fatalf("seed must run as root even when the pod sets runAsNonRoot: %v", security)
	}
	env := map[string]string{}
	for _, entry := range seed["env"].([]interface{}) {
		pair := entry.(map[string]interface{})
		env[pair["name"].(string)], _ = pair["value"].(string)
	}
	if env["SEED_UID"] != "1000" || env["SEED_GID"] != "2000" || env["LIMIT_KIB"] != "262144" || env["PR"] != "7" {
		t.Fatalf("env = %v", env)
	}
}

func TestResolveNFSConfigVolumeReadsClusterState(t *testing.T) {
	h := nfsTestHandler(t, prodDeployment(t), prodClaim("pv-1"), nfsPersistentVolume("pv-1"))
	got, err := h.resolveNFSConfigVolume(context.Background(), "myapp", nfsConfig(true))
	if err != nil {
		t.Fatal(err)
	}
	if got.VolumeName != "config" || got.Server != "nas.local" || got.Path != "/export/myapp" || got.PVName != "pv-1" {
		t.Fatalf("resolved = %+v", got)
	}
	if got.RunAsUser == nil || *got.RunAsUser != 1000 {
		t.Fatalf("runAsUser = %v", got.RunAsUser)
	}
	if got.SizeLimit.String() != "256Mi" {
		t.Fatalf("default size = %s", got.SizeLimit.String())
	}
}

func TestResolveNFSConfigVolumeWithoutCopyNeedsNoPV(t *testing.T) {
	h := nfsTestHandler(t, prodDeployment(t))
	got, err := h.resolveNFSConfigVolume(context.Background(), "myapp", nfsConfig(false))
	if err != nil || got == nil || got.Copy {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestResolveNFSConfigVolumeIgnoresOtherModes(t *testing.T) {
	h := nfsTestHandler(t)
	config := &v1.PreviewConfig{Spec: v1.PreviewConfigSpec{ConfigVolume: &v1.ConfigVolumeConfig{ClaimName: "myapp-config"}}}
	got, err := h.resolveNFSConfigVolume(context.Background(), "myapp", config)
	if got != nil || err != nil {
		t.Fatalf("snapshot mode: got %v, err %v", got, err)
	}
}

func TestResolveNFSConfigVolumeErrors(t *testing.T) {
	helm := nfsConfig(false)
	helm.Spec.DeploymentType = v1.DeploymentTypeHelm
	nonNFS := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-1"}}
	otherClaim := prodDeployment(t)
	otherClaim.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "other"
	initWritesConfig := prodDeployment(t)
	initWritesConfig.Spec.Template.Spec.InitContainers = []corev1.Container{{
		Name: "templater", Image: "busybox", VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/etc/myapp"}},
	}}
	zeroLimit := nfsConfig(false)
	zero := resource.MustParse("0")
	zeroLimit.Spec.ConfigVolume.NFS.SizeLimit = &zero

	cases := []struct {
		name    string
		objs    []client.Object
		config  *v1.PreviewConfig
		want    string
		notWant string
	}{
		{"helm", nil, helm, "mode: nfs supports raw-manifest Deployments only; Helm-rendered Deployments are not patched", ""},
		{"non-NFS PV", []client.Object{prodDeployment(t), prodClaim("pv-1"), nonNFS}, nfsConfig(true),
			`configVolume.claimName "myapp-config" is bound to PV "pv-1", which has no spec.nfs`, ""},
		{"unbound PVC", []client.Object{prodDeployment(t), prodClaim("")}, nfsConfig(true),
			`configVolume.claimName "myapp-config" in namespace "myapp" is not bound to a PV`, `PV ""`},
		{"volume not mounted", []client.Object{otherClaim}, nfsConfig(false),
			`is not used as a persistentVolumeClaim volume by Deployment "myapp"`, ""},
		{"init container uses the volume", []client.Object{initWritesConfig, prodClaim("pv-1"), nfsPersistentVolume("pv-1")}, nfsConfig(true),
			`init container "templater" of Deployment "myapp" mounts volume "config"`, ""},
		{"non-positive sizeLimit", []client.Object{prodDeployment(t)}, zeroLimit,
			`configVolume.nfs.sizeLimit "0" must be positive`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := nfsTestHandler(t, tc.objs...)
			_, err := h.resolveNFSConfigVolume(context.Background(), "myapp", tc.config)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if tc.notWant != "" && strings.Contains(err.Error(), tc.notWant) {
				t.Fatalf("error %q must not contain %q", err, tc.notWant)
			}
		})
	}
}

func TestProcessRejectsNFSMode(t *testing.T) {
	config := nfsConfig(false)
	config.Name, config.Namespace = "myapp", "myapp"
	h := nfsTestHandler(t, config)
	ks := &kustomizev1.Kustomization{ObjectMeta: metav1.ObjectMeta{Namespace: "preview-pr-7", Name: "myapp"}}
	err := h.Process(context.Background(), ks, "myapp")
	if err == nil || !strings.Contains(err.Error(), "mode: nfs is supported by the Kustomization flow only") {
		t.Fatalf("error = %v", err)
	}
}

func TestSetupConfigVolumeSkipsNFSMode(t *testing.T) {
	h := nfsTestHandler(t)
	if err := h.setupConfigVolume(context.Background(), "preview-pr-7", "myapp", "7", nfsConfig(true)); err != nil {
		t.Fatalf("nfs mode must create nothing: %v", err)
	}
}

// The snapshot Job script is part of a running flow; this literal is the script as it was
// before mode: nfs existed.
func TestSnapshotRewriteScriptGolden(t *testing.T) {
	want := "set -e\n" +
		"echo \"Rewriting URLs for preview namespace...\"\n" +
		"PROD_NS=\"myapp\"\n" +
		"PREVIEW_NS=\"preview-pr-7\"\n" +
		"DOMAIN=\"example.test\"\n" +
		"PR=\"7\"\n" +
		"APP=\"myapp\"\n" +
		"\n" +
		"# Install file command if not present\n" +
		"apk add --no-cache file >/dev/null 2>&1 || true\n" +
		"\n" +
		"find /config -type f | while read -r file; do\n" +
		"  if file \"$file\" | grep -qiE 'text|xml|json|ascii|utf-'; then\n" +
		"    # Rewrite internal FQDNs: .{prodNs}.svc.cluster.local -> .{previewNs}.svc.cluster.local\n" +
		"    sed -i \"s/\\.${PROD_NS}\\.svc\\.cluster\\.local/.${PREVIEW_NS}.svc.cluster.local/g\" \"$file\"\n" +
		"    # Rewrite external URLs: {app}.{domain} -> pr-{pr}-{app}.{domain}\n" +
		"    sed -i \"s/${APP}\\.${DOMAIN}/pr-${PR}-${APP}.${DOMAIN}/g\" \"$file\"\n" +
		"  fi\n" +
		"done\n" +
		"echo \"URL rewrite complete\"\n"
	if got := snapshotRewriteScript("myapp", "preview-pr-7", "example.test", "7"); got != want {
		t.Fatalf("snapshot script changed:\n%s", got)
	}
}

// skipOrFailInCI skips a docker-backed test on a laptop without a container runtime, and fails
// it under CI, where a skip would leave the seed script unproven.
func skipOrFailInCI(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf("%s; CI must run the seed script tests", reason)
	}
	t.Skip(reason)
}

// runSeedScript runs the seed script in alpine:3.21 (busybox), the image the init container uses.
func runSeedScript(t *testing.T, src, dst string, env ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		skipOrFailInCI(t, "docker is not available; the seed script needs busybox from alpine:3.21")
		return "", nil
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipOrFailInCI(t, "docker daemon is not running")
		return "", nil
	}
	args := []string{"run", "--rm", "-v", src + ":/src:ro", "-v", dst + ":/config",
		"-e", "PROD_NS=myapp", "-e", "PREVIEW_NS=preview-pr-7", "-e", "DOMAIN=example.test",
		"-e", "PR=7", "-e", "APP=myapp", "-e", "SOURCE=nas.local:/export/myapp",
		"-e", "LIMIT=256Mi", "-e", "LIMIT_KIB=262144"}
	for _, pair := range env {
		if strings.HasPrefix(pair, "--") {
			args = append(args, pair) // a docker flag
			continue
		}
		args = append(args, "-e", pair)
	}
	args = append(args, "alpine:3.21", "/bin/sh", "-c", nfsSeedScript)
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

func TestNFSSeedScriptRewritesOnceAndSkipsBinaries(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	text := "url=http://db.myapp.svc.cluster.local and https://myapp.example.test\n"
	if err := os.WriteFile(filepath.Join(src, "app.conf"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := []byte{0, 1, 2, 0xff}
	if err := os.WriteFile(filepath.Join(src, "blob.bin"), binary, 0o644); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		if out, err := runSeedScript(t, src, dst); err != nil {
			t.Fatalf("run %d: %v\n%s", run, err, out)
		}
	}
	got, err := os.ReadFile(filepath.Join(dst, "app.conf"))
	if err != nil {
		t.Fatal(err)
	}
	want := "url=http://db.preview-pr-7.svc.cluster.local and https://pr-7-myapp.example.test\n"
	if string(got) != want {
		t.Fatalf("app.conf = %q, want %q", got, want)
	}
	blob, err := os.ReadFile(filepath.Join(dst, "blob.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != string(binary) {
		t.Fatalf("binary file was modified: %v", blob)
	}
}

func TestNFSSeedScriptAbortsOverSizeLimit(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "big"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runSeedScript(t, src, dst, "LIMIT_KIB=1", "LIMIT=1Ki")
	if err == nil || !strings.Contains(out, "over sizeLimit 1Ki") {
		t.Fatalf("expected an over-limit abort, got err=%v out=%s", err, out)
	}
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Fatalf("nothing may be copied past the limit: %v", entries)
	}
}

// TestNFSSeedScriptFailsOnUnreadableFile drops the capabilities that let root read anything,
// which is what a root-squashed export looks like to the seed container.
func TestNFSSeedScriptFailsOnUnreadableFile(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for _, dir := range []string{src, dst} {
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "locked.conf"), []byte("secret\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	out, err := runSeedScript(t, src, dst, "--cap-drop=DAC_OVERRIDE", "--cap-drop=DAC_READ_SEARCH")
	if err == nil || !strings.Contains(out, "cannot copy") || !strings.Contains(out, "root-squashed") {
		t.Fatalf("expected a root-squash failure, got err=%v out=%s", err, out)
	}
}
