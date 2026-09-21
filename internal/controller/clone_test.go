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

// The clone is disposable and its volume is a copy-on-write child whose first
// writes are copy-ups krbd drains at under one per second; durability settings
// only make bring-up wait on that path. None of these are in CNPG's fixed
// parameter list, so the operator accepts them.
func TestBuildCloneClusterSpec_TradesDurabilityForBringUp(t *testing.T) {
	spec := buildCloneClusterSpec("vs", "30Gi", "sc", "", nil)
	params := spec["postgresql"].(map[string]interface{})["parameters"].(map[string]interface{})
	for _, p := range []string{"fsync", "synchronous_commit", "full_page_writes"} {
		if params[p] != "off" {
			t.Errorf("parameters[%s] = %v, want off", p, params[p])
		}
	}
	// CNPG rejects a spec that names one of its fixed parameters.
	for _, fixed := range []string{"wal_level", "hot_standby", "archive_mode", "listen_addresses", "port"} {
		if _, set := params[fixed]; set {
			t.Errorf("parameters sets fixed parameter %s, which CNPG refuses", fixed)
		}
	}
}

func sourceCluster(instances, ready int64, synchronous map[string]interface{}, minSync int64) *unstructured.Unstructured {
	c := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec":   map[string]interface{}{"instances": instances, "postgresql": map[string]interface{}{}},
		"status": map[string]interface{}{"readyInstances": ready},
	}}
	if synchronous != nil {
		c.Object["spec"].(map[string]interface{})["postgresql"].(map[string]interface{})["synchronous"] = synchronous
	}
	if minSync > 0 {
		c.Object["spec"].(map[string]interface{})["minSyncReplicas"] = minSync
	}
	return c
}

func TestColdSnapshotEligible(t *testing.T) {
	cases := []struct {
		name    string
		cluster *unstructured.Unstructured
		want    bool
	}{
		// CNPG's prefer-standby silently picks the primary when there is no
		// standby, and fencing the primary is an outage: a single instance must
		// take the raw path.
		{"single instance", sourceCluster(1, 1, nil, 0), false},
		// A declared standby that is not ready is not fenceable either.
		{"standby not ready", sourceCluster(2, 1, nil, 0), false},
		{"two ready, async", sourceCluster(2, 2, nil, 0), true},
		{"two ready, synchronous preferred", sourceCluster(2, 2, map[string]interface{}{"method": "any", "number": int64(1), "dataDurability": "preferred"}, 0), true},
		// dataDurability defaults to required: fencing the only standby would
		// block every commit on the primary for the length of the snapshot.
		{"synchronous, durability unset", sourceCluster(2, 2, map[string]interface{}{"method": "any", "number": int64(1)}, 0), false},
		{"synchronous required", sourceCluster(2, 2, map[string]interface{}{"method": "any", "number": int64(1), "dataDurability": "required"}, 0), false},
		{"legacy minSyncReplicas", sourceCluster(3, 3, nil, 1), false},
		{"three ready, async", sourceCluster(3, 3, nil, 0), true},
	}
	for _, tc := range cases {
		got, why := coldSnapshotEligible(tc.cluster)
		if got != tc.want {
			t.Errorf("%s: eligible = %v (%s), want %v", tc.name, got, why, tc.want)
		}
		if why == "" {
			t.Errorf("%s: no reason given", tc.name)
		}
	}
}

func TestBuildColdBackup_IsOfflineAndTargetsAStandby(t *testing.T) {
	b := buildColdBackup("postgres", "migcheck-warm-postgres-apps", "postgres-apps", map[string]string{"k": "v"})
	if b.GetKind() != "Backup" || b.GroupVersionKind().Group != "postgresql.cnpg.io" {
		t.Fatalf("kind = %s/%s, want postgresql.cnpg.io/Backup", b.GroupVersionKind().Group, b.GetKind())
	}
	if b.GetNamespace() != "postgres" || b.GetName() != "migcheck-warm-postgres-apps" {
		t.Errorf("name = %s/%s; CNPG names the PGDATA VolumeSnapshot after the Backup, so this must be the warm snapshot name", b.GetNamespace(), b.GetName())
	}
	if b.GetLabels()["k"] != "v" {
		t.Errorf("labels not carried: %v", b.GetLabels())
	}
	if got, _, _ := unstructured.NestedString(b.Object, "spec", "cluster", "name"); got != "postgres-apps" {
		t.Errorf("spec.cluster.name = %q", got)
	}
	if got, _, _ := unstructured.NestedString(b.Object, "spec", "method"); got != "volumeSnapshot" {
		t.Errorf("spec.method = %q, want volumeSnapshot", got)
	}
	// online:false is what makes the data directory a clean shutdown with
	// nothing to replay; it must override the source cluster's online default.
	online, found, _ := unstructured.NestedBool(b.Object, "spec", "online")
	if !found || online {
		t.Errorf("spec.online = %v (found=%v), want an explicit false", online, found)
	}
	if got, _, _ := unstructured.NestedString(b.Object, "spec", "target"); got != "prefer-standby" {
		t.Errorf("spec.target = %q, want prefer-standby", got)
	}
	// Same deep-copy requirement as the cluster spec: only JSON-compatible values.
	b.DeepCopy()
}
