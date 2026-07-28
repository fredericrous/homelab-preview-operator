package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestBuildCloneClusterSpec_PrefersOSDLocalNodes(t *testing.T) {
	spec := buildCloneClusterSpec("vs-1", "30Gi", "rook-ceph-block-encrypted", "img:16", nil)

	terms, found, err := unstructured.NestedSlice(spec,
		"affinity", "additionalPodAffinity", "preferredDuringSchedulingIgnoredDuringExecution")
	if err != nil || !found {
		t.Fatalf("no preferred pod affinity: found=%v err=%v", found, err)
	}
	if len(terms) != 1 {
		t.Fatalf("affinity terms = %d, want 1", len(terms))
	}
	term := terms[0].(map[string]interface{})

	pat := term["podAffinityTerm"].(map[string]interface{})
	if got := pat["topologyKey"]; got != "kubernetes.io/hostname" {
		t.Errorf("topologyKey = %v, want kubernetes.io/hostname (per-node locality)", got)
	}

	// Pod affinity defaults to the scheduled pod's own namespace — the throwaway
	// clone namespace, where no OSD runs. Without this the term matches nothing
	// and silently stops steering placement.
	ns := pat["namespaces"].([]interface{})
	if len(ns) != 1 || ns[0] != "rook-ceph" {
		t.Errorf("namespaces = %v, want [rook-ceph]", ns)
	}

	labels := pat["labelSelector"].(map[string]interface{})["matchLabels"].(map[string]interface{})
	if got := labels["app"]; got != "rook-ceph-osd" {
		t.Errorf("matchLabels[app] = %v, want rook-ceph-osd", got)
	}

	// Preferred, never required: an unschedulable clone is a worse failure than
	// a slow one.
	if _, required, _ := unstructured.NestedSlice(spec,
		"affinity", "additionalPodAffinity", "requiredDuringSchedulingIgnoredDuringExecution"); required {
		t.Error("affinity is required; must stay preferred so the clone still schedules when OSD nodes are full")
	}
}

// The spec is handed to unstructured.SetNestedMap, which rejects any value that
// is not a JSON-compatible type — a plain `int` weight would panic there at
// runtime, not at compile time.
func TestBuildCloneClusterSpec_IsDeepCopyable(t *testing.T) {
	spec := buildCloneClusterSpec("vs-1", "30Gi", "sc", "", nil)
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := unstructured.SetNestedMap(obj.Object, spec, "spec"); err != nil {
		t.Fatalf("spec is not settable on an unstructured object: %v", err)
	}
	obj.DeepCopy()
}

func TestBuildCloneClusterSpec_ImageSelection(t *testing.T) {
	withImage := buildCloneClusterSpec("vs", "1Gi", "sc", "ghcr.io/pg:17", map[string]interface{}{"name": "pg"})
	if withImage["imageName"] != "ghcr.io/pg:17" {
		t.Errorf("imageName = %v, want the explicit source image", withImage["imageName"])
	}
	if _, ok := withImage["imageCatalogRef"]; ok {
		t.Error("imageCatalogRef set alongside imageName; CNPG accepts only one")
	}

	catalogRef := map[string]interface{}{"name": "pg", "major": int64(17)}
	withCatalog := buildCloneClusterSpec("vs", "1Gi", "sc", "", catalogRef)
	if _, ok := withCatalog["imageName"]; ok {
		t.Error("imageName set when the source only had a catalog ref")
	}
	if withCatalog["imageCatalogRef"] == nil {
		t.Error("imageCatalogRef not carried over from the source cluster")
	}

	neither := buildCloneClusterSpec("vs", "1Gi", "sc", "", nil)
	if _, ok := neither["imageName"]; ok {
		t.Error("imageName set with no source image")
	}
	if _, ok := neither["imageCatalogRef"]; ok {
		t.Error("imageCatalogRef set with no source catalog ref")
	}
}

func TestBuildCloneClusterSpec_CarriesSourceStorage(t *testing.T) {
	// Restoring an encrypted snapshot into an unencrypted volume is rejected by
	// the CSI driver, so the clone must inherit the source storage class.
	spec := buildCloneClusterSpec("vs", "30Gi", "rook-ceph-block-encrypted", "", nil)
	storage := spec["storage"].(map[string]interface{})
	if storage["storageClass"] != "rook-ceph-block-encrypted" || storage["size"] != "30Gi" {
		t.Errorf("storage = %v, want the source class and size verbatim", storage)
	}
}
